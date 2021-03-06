package fetcher

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/dcos/dcos-diagnostics/dcos"
	"github.com/dcos/dcos-diagnostics/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func Test_NewReturnErrorWhenCantCreateZip(t *testing.T) {
	mockHistogram := &mocks.MockHistogram{}
	_, err := New("not_existing_dir", nil, nil, nil, nil, mockHistogram)
	assert.Contains(t, err.Error(), "could not create temp zip file in not_existing_dir")
}

func Test_FetcherReturnEmptyZipOnClosedContext(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.TODO())
	cancelFunc()

	output := make(chan BulkResponse)
	mockHistogram := &mocks.MockHistogram{}

	f, err := New("", nil, nil, nil, output, mockHistogram)
	assert.NoError(t, err)
	go f.Run(ctx)

	zipfile := <-output

	z, err := zip.OpenReader(zipfile.ZipFilePath)
	assert.NoError(t, err)
	assert.Empty(t, z.File)
}

func Test_FetcherShouldSentUpdateAfterFetchingAnEndpoint(t *testing.T) {
	input := make(chan EndpointRequest)
	statusUpdate := make(chan StatusUpdate)
	output := make(chan BulkResponse)

	server, _ := stubServer("/ping", "pong")
	host := "http://" + server.URL[7:]
	defer server.Close()

	observer := &mocks.MockObserver{}
	observer.On("Observe", mock.MatchedBy(func(v float64) bool { return v > 0 })).Once()
	mockHistogram := &mocks.MockHistogram{}
	mockHistogram.On("WithLabelValues", "/ping", "200").Return(observer).Once()

	f, err := New("", http.DefaultClient, input, statusUpdate, output, mockHistogram)
	assert.NoError(t, err)
	go f.Run(context.TODO())

	input <- EndpointRequest{
		URL:      host + "/ping",
		Node:     dcos.Node{IP: "127.0.0.1", Role: dcos.AgentRole},
		FileName: "ping_file",
	}

	assert.Equal(t, StatusUpdate{URL: host + "/ping"}, <-statusUpdate)

	input <- EndpointRequest{
		URL:      host + "/optional",
		Node:     dcos.Node{IP: "127.0.0.2", Role: dcos.MasterRole},
		FileName: "optional-file",
		Optional: true,
	}

	assert.Equal(t, StatusUpdate{URL: host + "/optional"}, <-statusUpdate)

	input <- EndpointRequest{
		URL:      host + "/error",
		Node:     dcos.Node{IP: "127.0.0.2", Role: dcos.MasterRole},
		FileName: "error_file",
	}

	status := <-statusUpdate
	assert.Equal(t, host+"/error", status.URL)
	assert.Contains(t, status.Error.Error(), "Return code 404. Body: 404 page not found")

	close(input)

	zipfile := <-output

	z, err := zip.OpenReader(zipfile.ZipFilePath)
	require.NoError(t, err)
	assert.Len(t, z.File, 1)

	rc, err := z.File[0].Open()
	require.NoError(t, err)

	r, err := gzip.NewReader(rc)
	require.NoError(t, err)

	body, err := ioutil.ReadAll(r)
	require.NoError(t, err)

	assert.Equal(t, "pong", string(body))

	mockHistogram.AssertExpectations(t)
	observer.AssertExpectations(t)
}

// http://keighl.com/post/mocking-http-responses-in-golang/
func stubServer(uri string, body string) (*httptest.Server, *http.Transport) {
	return mockServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() == uri {
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")

			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			defer gz.Close()
			_, err := gz.Write([]byte(body))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		} else {
			http.NotFound(w, r)
		}
	})
}

func mockServer(handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *http.Transport) {
	server := httptest.NewServer(http.HandlerFunc(handle))

	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			return url.Parse(server.URL)
		},
	}

	return server, transport
}
