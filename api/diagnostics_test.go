package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/dcos/dcos-diagnostics/dcos"
	"github.com/dcos/dcos-diagnostics/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticsJobInitReturnsErrorWhenConfigurationIsInvalid(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}

	// file does not exist
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{"test_endpoints-config.json"}

	err := job.Init()
	assert.Error(t, err) // we can't use ErrorEqual: system errors differ between unix and windows
	assert.Contains(t, err.Error(), "could not init diagnostic job: could not initialize external log providers: could not read test_endpoints-config.json: open test_endpoints-config.json: ")

	// file exists but is not valid JSON
	tmpfile, err := ioutil.TempFile("", "test_endpoints-config.json")
	defer os.Remove(tmpfile.Name())
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{tmpfile.Name()}

	err = job.Init()
	assert.Contains(t, err.Error(), "could not init diagnostic job: could not initialize external log providers: could not parse ")
}

func TestDiagnosticsJobInitWithValidFilesCheckIfConfigsAreMerged(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{
		filepath.Join("testdata", "endpoint-config.json"),
		filepath.Join("testdata", "endpoint-config-3.json"),
		filepath.Join("testdata", "endpoint-config-2.json"),
	}

	err := job.Init()
	assert.NoError(t, err)

	assert.Equal(t, float32(-1), job.getJobProgressPercentage())
	httpProviders := map[string]HTTPProvider{
		"5050-__processes__.json":         {Port: 5050, URI: "/__processes__", Role: []string{"master"}},
		"5050-master_state-summary.json":  {Port: 5050, URI: "/master/state-summary", Role: []string{"master"}},
		"5050-registrar_1__registry.json": {Port: 5050, URI: "/registrar(1)/registry", Role: []string{"master"}},
		"5050-system_stats_json.json":     {Port: 5050, URI: "/system/stats.json", Role: []string{"master"}},
		"5051-__processes__.json":         {Port: 5051, URI: "/__processes__", Role: []string{"agent", "agent_public"}},
		"5051-metrics_snapshot.json":      {Port: 5051, URI: "/metrics/snapshot", Role: []string{"agent", "agent_public"}},
		"5051-system_stats_json.json":     {Port: 5051, URI: "/system/stats.json", Role: []string{"agent", "agent_public"}},
		"dcos-diagnostics-health.json":    {Port: 1050, URI: "/system/health/v1", FileName: "dcos-diagnostics-health.json"},
		"dcos-download.service":           {Port: 1050, URI: "/system/health/v1/logs/units/dcos-download.service", FileName: "dcos-download.service"},
		"dcos-link-env.service":           {Port: 1050, URI: "/system/health/v1/logs/units/dcos-link-env.service", FileName: "dcos-link-env.service"},
		"dcos-setup.service":              {Port: 1050, URI: "/system/health/v1/logs/units/dcos-setup.service", FileName: "dcos-setup.service"},
		"unit_a":                          {Port: 1050, URI: "/system/health/v1/logs/units/unit_a", FileName: "unit_a"},
		"unit_b":                          {Port: 1050, URI: "/system/health/v1/logs/units/unit_b", FileName: "unit_b"},
		"unit_c":                          {Port: 1050, URI: "/system/health/v1/logs/units/unit_c", FileName: "unit_c"},
		"unit_to_fail":                    {Port: 1050, URI: "/system/health/v1/logs/units/unit_to_fail", FileName: "unit_to_fail"},
		"uri_not_avail.txt":               {Port: 5050, URI: "/storage/uri_not_avail", Role: []string{"master"}, FileName: "uri_not_avail.txt", Optional: true},
	}

	assert.Equal(t, httpProviders, job.logProviders.HTTPEndpoints)
	assert.Equal(t, map[string]FileProvider{
		"opt_mesosphere_active.buildinfo.full.json":      {Location: "/opt/mesosphere/active.buildinfo.full.json", Role: []string{"agent", "agent_public"}},
		"var_lib_dcos_exhibitor_conf_zoo.cfg":            {Location: "/var/lib/dcos/exhibitor/conf/zoo.cfg", Role: []string{"master"}},
		"var_lib_dcos_exhibitor_zookeeper_snapshot_myid": {Location: "/var/lib/dcos/exhibitor/zookeeper/snapshot/myid", Role: []string{"master"}},
		"not_existing_file":                              {Location: "/not/existing/file", Optional: true},
	}, job.logProviders.LocalFiles)

	assert.Equal(t, map[string]CommandProvider{
		"binsh_-c_cat etc*-release.output": {Command: []string{"/bin/sh", "-c", "cat /etc/*-release"}},
		"dmesg_-T.output":                  {Command: []string{"dmesg", "-T"}},
		"echo_OK.output":                   {Command: []string{"echo", "OK"}},
		"optmesospherebincurl_-s_-S_http:localhost:62080v1vips.output": {
			Command: []string{"/opt/mesosphere/bin/curl", "-s", "-S", "http://localhost:62080/v1/vips"},
			Role:    []string{"agent", "agent_public"},
		},
		"ps_aux_ww_Z.output":                {Command: []string{"ps", "aux", "ww", "Z"}},
		"systemctl_list-units_dcos*.output": {Command: []string{"systemctl", "list-units", "dcos*"}},
		"does_not_exist.output":             {Command: []string{"does", "not", "exist"}, Optional: true},
		"does_not_exist_required.output":    {Command: []string{"does", "not", "exist", "required"}, Optional: false},
	}, job.logProviders.LocalCommands)

}

func TestDiagnosticsJobInitWithValidFilesCheckIfConfigsAreMergedWithOrder(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{
		filepath.Join("testdata", "endpoint-config.json"),
		filepath.Join("testdata", "endpoint-config-2.json"),
		filepath.Join("testdata", "endpoint-config-3.json"), // « Overrides endpoint-config-2.json
	}

	err := job.Init()
	assert.NoError(t, err)

	assert.Equal(t, map[string]FileProvider{
		"opt_mesosphere_active.buildinfo.full.json":      {Location: "/opt/mesosphere/active.buildinfo.full.json", Role: []string{"agent", "agent_public"}},
		"var_lib_dcos_exhibitor_conf_zoo.cfg":            {Location: "/var/lib/dcos/exhibitor/conf/zoo.cfg", Role: []string{"master"}},
		"var_lib_dcos_exhibitor_zookeeper_snapshot_myid": {Location: "/var/lib/dcos/exhibitor/zookeeper/snapshot/myid", Role: []string{"master"}},
		"not_existing_file":                              {Location: "/not/existing/file", Optional: false}, // « Here is the difference
	}, job.logProviders.LocalFiles)
}

func TestGetLogsEndpoints(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	// Check if double entries are merged
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{
		filepath.Join("testdata", "endpoint-config.json"),
		filepath.Join("testdata", "endpoint-config-2.json"),
		filepath.Join("testdata", "endpoint-config-3.json"),
	}

	err := job.Init()
	require.NoError(t, err)

	endpoints, err := job.getLogsEndpoints()
	assert.NoError(t, err)

	const logPath = ":1050/system/health/v1/logs/"
	assert.Equal(t, endpoints, func() (x map[string]endpointSpec) {
		x = make(map[string]endpointSpec)
		for k, v := range map[string]string{
			"/var/lib/dcos/exhibitor/conf/zoo.cfg":            logPath + "files/var_lib_dcos_exhibitor_conf_zoo.cfg",
			"/var/lib/dcos/exhibitor/zookeeper/snapshot/myid": logPath + "files/var_lib_dcos_exhibitor_zookeeper_snapshot_myid",
			"5050-__processes__.json":                         ":5050/__processes__",
			"5050-master_state-summary.json":                  ":5050/master/state-summary",
			"5050-registrar_1__registry.json":                 ":5050/registrar(1)/registry",
			"5050-system_stats_json.json":                     ":5050/system/stats.json",
			"binsh_-c_cat etc*-release.output":                logPath + "cmds/binsh_-c_cat etc*-release.output",
			"dcos-diagnostics-health.json":                    ":1050/system/health/v1",
			"dcos-download.service":                           logPath + "units/dcos-download.service",
			"dcos-link-env.service":                           logPath + "units/dcos-link-env.service",
			"dcos-setup.service":                              logPath + "units/dcos-setup.service",
			"dmesg_-T.output":                                 logPath + "cmds/dmesg_-T.output",
			"echo_OK.output":                                  logPath + "cmds/echo_OK.output",
			"ps_aux_ww_Z.output":                              logPath + "cmds/ps_aux_ww_Z.output",
			"systemctl_list-units_dcos*.output":               logPath + "cmds/systemctl_list-units_dcos*.output",
			"unit_a":                                          logPath + "units/unit_a",
			"unit_b":                                          logPath + "units/unit_b",
			"unit_c":                                          logPath + "units/unit_c",
			"unit_to_fail":                                    logPath + "units/unit_to_fail",
			"/not/existing/file":                              logPath + "files/not_existing_file",
			"does_not_exist.output":                           logPath + "cmds/does_not_exist.output",
		} {
			x[k] = endpointSpec{PortAndPath: v}
		}
		x["uri_not_avail.txt"] = endpointSpec{PortAndPath: ":5050/storage/uri_not_avail", Optional: true}
		x["does_not_exist_required.output"] = endpointSpec{PortAndPath: logPath + "cmds/does_not_exist_required.output"}
		return
	}(), "only endpoints for master role should appear here")
}

func TestDispatchLogsForCommand(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config.json")}

	err := job.Init()
	require.NoError(t, err)

	r, err := job.dispatchLogs(context.TODO(), "cmds", "echo_OK.output")
	assert.NoError(t, err)

	data, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "OK\n", string(data))
}

func TestDispatchLogsForCommandThatNotExistsButIsOptional(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config-2.json")}

	err := job.Init()
	require.NoError(t, err)

	r, err := job.dispatchLogs(context.TODO(), "cmds", "does_not_exist.output")
	require.NoError(t, err)

	data, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	assert.Contains(t, string(data), `exec: "does": executable file not found in `)
}

func TestDispatchLogsForCommandThatNotExistsAndIsRequired(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config.json")}

	err := job.Init()
	require.NoError(t, err)

	_, err = job.dispatchLogs(context.TODO(), "cmds", "does_not_exist_required.output")
	assert.Error(t, err)
}

func TestDispatchLogsForFiles(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config.json")}

	f, err := ioutil.TempFile("", "")
	require.NoError(t, err)
	_, err = f.WriteString("OK")
	require.NoError(t, err)
	defer os.Remove(f.Name())

	job.logProviders.LocalFiles = map[string]FileProvider{"ok": {Location: f.Name()}}

	r, err := job.dispatchLogs(context.TODO(), "files", "ok")
	assert.NoError(t, err)

	data, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "OK", string(data))
}

func TestDispatchLogsForOptionalFileThatNotExists(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config-2.json")}

	err := job.Init()
	require.NoError(t, err)

	r, err := job.dispatchLogs(context.TODO(), "files", "not_existing_file")
	require.NoError(t, err)

	data, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	assert.Contains(t, string(data), "open /not/existing/file: ")
}

func TestDispatchLogsForUnit(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip()
	}

	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config.json")}

	err := job.Init()
	require.NoError(t, err)

	r, err := job.dispatchLogs(context.TODO(), "units", "unit_a")
	assert.NoError(t, err)

	data, err := ioutil.ReadAll(r)
	require.NoError(t, err)
	assert.Empty(t, data)
}

func TestDispatchLogsForUnit_Windows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip()
	}

	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}
	job.Cfg.FlagDiagnosticsBundleEndpointsConfigFiles = []string{filepath.Join("testdata", "endpoint-config.json")}

	err := job.Init()
	require.NoError(t, err)

	r, err := job.dispatchLogs(context.TODO(), "units", "unit_a")
	assert.Nil(t, r)
	assert.EqualError(t, err, "there is no journal on Windows")
}

func TestDispatchLogsWithUnknownProvider(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}

	r, err := job.dispatchLogs(context.TODO(), "unknown", "echo_OK.output")
	assert.EqualError(t, err, "Unknown provider unknown")
	assert.Nil(t, r)
}

func TestDispatchLogsWithUnknownEntity(t *testing.T) {
	job := DiagnosticsJob{Cfg: testCfg(), DCOSTools: &fakeDCOSTools{}}

	for _, provider := range []string{"cmds", "files", "units"} {
		r, err := job.dispatchLogs(context.TODO(), provider, "unknown-entity")
		assert.EqualError(t, err, "Not found unknown-entity")
		assert.Nil(t, r)
	}
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

func TestFindRequestedNodes(t *testing.T) {
	tools := new(MockedTools)

	tools.On("GetMasterNodes").Return(
		[]dcos.Node{
			{IP: "10.10.0.1", Role: "master"},
			{IP: "10.10.0.2", Host: "my-host.com", Role: "master"},
			{IP: "10.10.0.3", Role: "master", MesosID: "12345-12345"},
		}, nil)
	tools.On("GetAgentNodes").Return([]dcos.Node{{IP: "127.0.0.1", Role: "agent"}}, nil)

	var tests = []struct {
		requestedNodes []string
		expectedNodes  []dcos.Node
	}{
		{[]string{"all"}, []dcos.Node{
			{IP: "10.10.0.1", Role: "master"},
			{IP: "10.10.0.2", Role: "master", Host: "my-host.com"},
			{IP: "10.10.0.3", Role: "master", MesosID: "12345-12345"},
			{IP: "127.0.0.1", Role: "agent"},
		}},
		{[]string{"masters"}, []dcos.Node{
			{IP: "10.10.0.1", Role: "master"},
			{IP: "10.10.0.2", Role: "master", Host: "my-host.com"},
			{IP: "10.10.0.3", Role: "master", MesosID: "12345-12345"},
		}},
		{[]string{"agents"}, []dcos.Node{
			{IP: "127.0.0.1", Role: "agent"},
		}},
		{[]string{"10.10.0.1"}, []dcos.Node{
			{IP: "10.10.0.1", Role: "master"},
		}},
		{[]string{"my-host.com"}, []dcos.Node{
			{IP: "10.10.0.2", Role: "master", Host: "my-host.com"},
		}},
		{[]string{"12345-12345"}, []dcos.Node{
			{IP: "10.10.0.3", Role: "master", MesosID: "12345-12345"},
		}},
		{[]string{"agents", "10.10.0.1"}, []dcos.Node{
			{IP: "127.0.0.1", Role: "agent"},
			{IP: "10.10.0.1", Role: "master"},
		}},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.requestedNodes, "_"), func(t *testing.T) {
			actualNodes, err := findRequestedNodes(tt.requestedNodes, tools)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedNodes, actualNodes)
		})
	}

	tools.AssertExpectations(t)
}

func TestFindRequestedNodesError(t *testing.T) {
	var tests = []struct {
		requestedNodes []string
		expectedErr    string
		masterErr      error
		agentErr       error
	}{
		{[]string{"all"}, "can't find any nodes", nil, nil},
		{[]string{}, "no nodes were requested", nil, nil},
		{[]string{""}, "can't find any nodes", nil, nil},
		{[]string{}, "could not get master nodes: x", errors.New("x"), nil},
		{[]string{}, "could not get agent nodes: y", nil, errors.New("y")},
		{[]string{}, "could not get master nodes: x", errors.New("x"), errors.New("y")},
	}

	for _, tt := range tests {
		t.Run(tt.expectedErr+ "_" + strings.Join(tt.requestedNodes, "_"), func(t *testing.T) {
			tools := new(MockedTools)
			tools.On("GetMasterNodes").Return([]dcos.Node{}, tt.masterErr)
			tools.On("GetAgentNodes").Return([]dcos.Node{}, tt.agentErr).Maybe()

			actualNodes, err := findRequestedNodes(tt.requestedNodes, tools)

			require.EqualError(t, err, tt.expectedErr)
			assert.Empty(t, actualNodes)
			tools.AssertExpectations(t)
		})
	}
}

func TestGetStatus(t *testing.T) {
	tools := &fakeDCOSTools{}
	config := testCfg()

	parameters := []struct {
		job      *DiagnosticsJob
		expected bundleReportStatus
	}{
		{
			&DiagnosticsJob{Running: true, Cfg: config, DCOSTools: tools,
				JobStarted: time.Date(2019, 9, 1, 4, 45, 0, 0, time.UTC),
			},
			bundleReportStatus{
				Running:                        true,
				JobStarted:                     "2019-09-01 04:45:00 +0000 UTC",
				Errors:                         []string{},
				DiagnosticBundlesBaseDir:       config.FlagDiagnosticsBundleDir,
				DiagnosticsJobTimeoutMin:       config.FlagDiagnosticsJobTimeoutMinutes,
				DiagnosticsUnitsLogsSinceHours: config.FlagDiagnosticsBundleUnitsLogsSinceString,
			},
		}, {
			&DiagnosticsJob{Running: false, Cfg: config, DCOSTools: tools,
				JobStarted: time.Date(2019, 9, 1, 4, 45, 0, 0, time.UTC),
				JobEnded:   time.Date(2019, 9, 1, 5, 00, 0, 0, time.UTC),
			},
			bundleReportStatus{
				Running:                        false,
				JobStarted:                     "2019-09-01 04:45:00 +0000 UTC",
				JobEnded:                       "2019-09-01 05:00:00 +0000 UTC",
				JobDuration:                    "15m0s",
				Errors:                         []string{},
				DiagnosticBundlesBaseDir:       config.FlagDiagnosticsBundleDir,
				DiagnosticsJobTimeoutMin:       config.FlagDiagnosticsJobTimeoutMinutes,
				DiagnosticsUnitsLogsSinceHours: config.FlagDiagnosticsBundleUnitsLogsSinceString,
			},
		},
	}

	for _, i := range parameters {
		actual := i.job.getBundleReportStatus()
		i.expected.DiskUsedPercent = actual.DiskUsedPercent //TODO(janisz): Inject disk.Usage to DiagnosticsJob so this could be tested.
		assert.Equal(t, i.expected, actual)
	}
}

func TestGetStatusWhenJobIsRunning(t *testing.T) {
	tools := new(MockedTools)

	called := make(chan bool)
	wait := make(chan bool)

	stubServer := func() (*httptest.Server, *http.Transport) {
		return mockServer(func(w http.ResponseWriter, r *http.Request) {
			called <- true
			t.Logf("Called %s", r.URL.RequestURI())
			<-wait
			w.WriteHeader(200)
		})
	}

	server, _ := stubServer()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.URL[7:])

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.1:1050%s/logs", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte(`{"slow_server": {"PortAndPath":":`+port+`"}}`), http.StatusOK, nil)
	tools.On("GetNodeRole").Return("master", nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetAgentNodes").Return([]dcos.Node{{IP: "127.0.0.1", Role: "master"}}, nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{{Leader: true, IP: "127.0.0.1", Role: "master"}}, nil)

	cfg := testCfg()
	cfg.FlagDiagnosticsBundleFetchersCount = 1

	mockObs := &mocks.MockObserver{}
	mockObs.On("Observe", mock.MatchedBy(func(v float64) bool {
		return v > 0
	})).Maybe()
	mockHistogram := &mocks.MockHistogram{}
	mockHistogram.On("WithLabelValues", "", "200").Return(mockObs).Maybe()

	dt := &Dt{
		Cfg:              cfg,
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: cfg, DCOSTools: tools, client: http.DefaultClient, FetchPrometheusVector: mockHistogram},
		MR:               &MonitoringResponse{},
	}

	dt.DtDiagnosticsJob.run(bundleCreateRequest{Nodes: []string{"all"}})

	<-called
	for {
		t.Logf("Get status")
		status := dt.DtDiagnosticsJob.getBundleReportStatus()
		if status.Running {
			t.Logf("Job is running")
			break
		}
	}
	wait <- false

	tools.AssertExpectations(t)
	mockObs.AssertExpectations(t)
	mockHistogram.AssertExpectations(t)
}

func TestCreateBundle(t *testing.T) {
	tools := new(MockedTools)

	stubServer := func() (*httptest.Server, *http.Transport) {
		return mockServer(func(w http.ResponseWriter, r *http.Request) {
			t.Logf("Called %s", r.URL.RequestURI())
			if r.URL.Path == "/ping" {
				time.Sleep(time.Millisecond)
				w.Write([]byte("pong"))
			} else {
				http.NotFound(w, r)
			}
		})
	}

	server, _ := stubServer()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.URL[7:])

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.1:1050%s/logs", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte(`{"ping": {"PortAndPath":":`+port+`/ping"}}`), http.StatusOK, nil)
	tools.On("GetNodeRole").Return("master", nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetAgentNodes").Return([]dcos.Node{}, nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{{Leader: true, IP: "127.0.0.1", Role: "master"}}, nil)

	cfg := testCfg()
	mockObs := &mocks.MockObserver{}
	mockObs.On("Observe", mock.MatchedBy(func(v float64) bool {
		return v > 0
	})).Once()
	mockHistogram := &mocks.MockHistogram{}
	mockHistogram.On("WithLabelValues", "/ping", "200").Return(mockObs).Once()
	job := &DiagnosticsJob{Cfg: cfg, DCOSTools: tools, client: http.DefaultClient, FetchPrometheusVector: mockHistogram}
	dt := &Dt{
		Cfg:              cfg,
		DtDCOSTools:      tools,
		DtDiagnosticsJob: job,
		MR:               &MonitoringResponse{},
	}

	_, err := dt.DtDiagnosticsJob.run(bundleCreateRequest{Nodes: []string{"all"}})
	require.NoError(t, err)

	for job.getBundleReportStatus().Running {
		t.Log("Waiting for job to end")
		time.Sleep(10 * time.Microsecond)
	}

	status := job.getBundleReportStatus()

	assert.False(t, status.Running)
	assert.Equal(t, float32(100.0), status.JobProgressPercentage)
	assert.Equal(t, "Diagnostics job successfully collected all data", status.Status)
	assert.Empty(t, status.Errors)
	assert.Empty(t, status.Errors)

	reader, err := zip.OpenReader(status.LastBundlePath)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join("127.0.0.1_master", "ping"), reader.File[0].Name)
	assert.Equal(t, "summaryReport.txt", reader.File[1].Name)

	rc, err := reader.File[0].Open()
	require.NoError(t, err)
	content, err := ioutil.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "pong", string(content))

	rc, err = reader.File[1].Open()
	require.NoError(t, err)
	content, err = ioutil.ReadAll(rc)
	require.NoError(t, err)
	assert.Contains(t, string(content), "GET http://127.0.0.1:")

	tools.AssertExpectations(t)
	mockObs.AssertExpectations(t)
	mockHistogram.AssertExpectations(t)
}

func TestCancelWhenJobIsRunning(t *testing.T) {
	tools := new(MockedTools)

	mockHistogram := &mocks.MockHistogram{}

	called := make(chan bool)
	wait := make(chan bool)

	stubServer := func() (*httptest.Server, *http.Transport) {
		return mockServer(func(w http.ResponseWriter, r *http.Request) {
			called <- true
			t.Logf("Called %s", r.URL.RequestURI())
			w.WriteHeader(200)
			<-wait
		})
	}

	server, _ := stubServer()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.URL[7:])

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.1:1050%s/logs", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte(`{"slow_server": {"PortAndPath":":`+port+`"}}`), http.StatusOK, nil)
	tools.On("GetNodeRole").Return("master", nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetAgentNodes").Return([]dcos.Node{}, nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{{Leader: true, IP: "127.0.0.1", Role: "master"}}, nil)

	cfg := testCfg()
	job := &DiagnosticsJob{Cfg: cfg, DCOSTools: tools, client: http.DefaultClient, FetchPrometheusVector: mockHistogram}
	dt := &Dt{
		Cfg:              cfg,
		DtDCOSTools:      tools,
		DtDiagnosticsJob: job,
		MR:               &MonitoringResponse{},
	}

	_, err := dt.DtDiagnosticsJob.run(bundleCreateRequest{Nodes: []string{"all"}})
	require.NoError(t, err)

	<-called
	require.True(t, job.getBundleReportStatus().Running)
	require.Empty(t, job.getBundleReportStatus().JobEnded, "job is running, end time should be empty")
	_, err = dt.DtDiagnosticsJob.cancel()
	require.NoError(t, err)

	for job.getBundleReportStatus().Running {
		t.Log("Waiting for job to end")
		time.Sleep(10 * time.Microsecond)
	}

	wait <- false

	status := job.getBundleReportStatus()

	assert.False(t, status.Running)
	assert.Equal(t, float32(100.0), status.JobProgressPercentage)
	assert.Equal(t, "Diagnostics job failed", status.Status)
	assert.NotEmpty(t, status.Errors)
	assert.NotEmpty(t, status.JobEnded, "job has finished, end time should not be empty")

	tools.AssertExpectations(t)
	mockHistogram.AssertExpectations(t)
}

func TestGetAllStatusWithRemoteCall(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	mockedResponse := `
			{
			  "is_running":true,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}`

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.1:1050%s/report/diagnostics/status", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte(mockedResponse), http.StatusOK, nil)
	tools.On("DetectIP").Return("", fmt.Errorf("some error"))
	tools.On("GetMasterNodes").Return([]dcos.Node{{Leader: true, IP: "127.0.0.1", Role: "master"}}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()
	require.NoError(t, err)
	assert.Contains(t, status, "127.0.0.1")
	assert.Equal(t, status["127.0.0.1"], bundleReportStatus{
		Running:                                  true,
		Status:                                   "MyStatus",
		LastBundlePath:                           "/path/to/snapshot",
		JobStarted:                               "0001-01-01 00:00:00 +0000 UTC",
		JobDuration:                              "2s",
		DiagnosticBundlesBaseDir:                 "/home/core/1",
		DiagnosticsJobTimeoutMin:                 720,
		DiskUsedPercent:                          28.0,
		DiagnosticsUnitsLogsSinceHours:           "24",
		DiagnosticsJobGetSingleURLTimeoutMinutes: 5,
		CommandExecTimeoutSec:                    10,
	})

	tools.AssertExpectations(t)
}

func TestGetAllStatusWhenNoMaterFoundShouldReturnError(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	tools.On("GetMasterNodes").Return([]dcos.Node{}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()

	assert.EqualError(t, err, "could not find any master")
	assert.Nil(t, status)
	tools.AssertExpectations(t)
}

func TestGetAllStatusWithLocalAnd503RemoteCall(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.2:1050%s/report/diagnostics/status", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte{}, http.StatusServiceUnavailable, nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{
		{Leader: false, IP: "127.0.0.1", Role: "master"},
		{Leader: true, IP: "127.0.0.2", Role: "master"}}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()
	assert.EqualError(t, err, "could not determine whether the diagnostics job is running or not: [could not get data from http://127.0.0.2:1050/system/health/v1/report/diagnostics/status got 503 status]")
	assert.Len(t, status, 1)
	assert.Contains(t, status, "127.0.0.1")

	tools.AssertExpectations(t)
}

func TestGetAllStatusWithLocalAndRemoteCallReturnsInvalidJson(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.2:1050%s/report/diagnostics/status", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte("not a json"), http.StatusOK, nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{
		{Leader: false, IP: "127.0.0.1", Role: "master"},
		{Leader: true, IP: "127.0.0.2", Role: "master"}}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()
	assert.EqualError(t, err, "could not determine whether the diagnostics job is running or not: [could not determine job status for master 127.0.0.2: invalid character 'o' in literal null (expecting 'u')]")
	assert.Len(t, status, 1)
	assert.Contains(t, status, "127.0.0.1")

	tools.AssertExpectations(t)
}

func TestGetAllStatusWithLocalAndFailingRemoteCall(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.2:1050%s/report/diagnostics/status", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte{}, http.StatusOK, errors.New("some error"))
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{
		{Leader: false, IP: "127.0.0.1", Role: "master"},
		{Leader: true, IP: "127.0.0.2", Role: "master"}}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()
	assert.EqualError(t, err, "could not determine whether the diagnostics job is running or not: [could not get data from http://127.0.0.2:1050/system/health/v1/report/diagnostics/status: some error]")
	assert.Len(t, status, 1)
	assert.Contains(t, status, "127.0.0.1")

	tools.AssertExpectations(t)
}

func TestGetAllStatusWithLocalAndRemoteCall(t *testing.T) {
	config := testCfg()

	tools := new(MockedTools)

	mockedResponse := `
			{
			  "is_running":true,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}`

	tools.On("Get",
		mock.MatchedBy(func(url string) bool {
			return url == fmt.Sprintf("http://127.0.0.2:1050%s/report/diagnostics/status", baseRoute)
		}),
		mock.MatchedBy(func(t time.Duration) bool { return t == 3*time.Second }),
	).Return([]byte(mockedResponse), http.StatusOK, nil)
	tools.On("DetectIP").Return("127.0.0.1", nil)
	tools.On("GetMasterNodes").Return([]dcos.Node{
		{Leader: false, IP: "127.0.0.1", Role: "master"},
		{Leader: true, IP: "127.0.0.2", Role: "master"}}, nil)

	job := &DiagnosticsJob{Cfg: config, DCOSTools: tools}

	status, err := job.getStatusAll()
	require.NoError(t, err)
	assert.Len(t, status, 2)
	assert.Contains(t, status, "127.0.0.1")
	assert.Contains(t, status, "127.0.0.2")
	assert.Equal(t, status["127.0.0.2"], bundleReportStatus{
		Running:                                  true,
		Status:                                   "MyStatus",
		LastBundlePath:                           "/path/to/snapshot",
		JobStarted:                               "0001-01-01 00:00:00 +0000 UTC",
		JobDuration:                              "2s",
		DiagnosticBundlesBaseDir:                 "/home/core/1",
		DiagnosticsJobTimeoutMin:                 720,
		DiskUsedPercent:                          28.0,
		DiagnosticsUnitsLogsSinceHours:           "24",
		DiagnosticsJobGetSingleURLTimeoutMinutes: 5,
		CommandExecTimeoutSec:                    10,
	})

	tools.AssertExpectations(t)
}

func TestIsSnapshotAvailable(t *testing.T) {
	tools := &fakeDCOSTools{}
	cfg := testCfg()
	defer os.RemoveAll(cfg.FlagDiagnosticsBundleDir)
	job := &DiagnosticsJob{Cfg: cfg, DCOSTools: tools}

	url := fmt.Sprintf("http://127.0.0.1:1050%s/report/diagnostics/list", baseRoute)
	mockedResponse := `[{"file_name": "/system/health/v1/report/diagnostics/serve/bundle-2016-05-13T22:11:36.zip", "file_size": 123}]`

	tools.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)

	validFilePath := filepath.Join(cfg.FlagDiagnosticsBundleDir, "bundle-local.zip")
	_, err := os.Create(validFilePath)
	require.NoError(t, err)
	invalidFilePath := filepath.Join(cfg.FlagDiagnosticsBundleDir, "local-bundle.zip")
	_, err = os.Create(invalidFilePath)
	require.NoError(t, err)

	host, remoteSnapshot, ok, err := job.isBundleAvailable("bundle-2016-05-13T22:11:36.zip")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, host, "127.0.0.1")
	assert.Equal(t, remoteSnapshot, "/system/health/v1/report/diagnostics/serve/bundle-2016-05-13T22:11:36.zip")

	host, remoteSnapshot, ok, err = job.isBundleAvailable("bundle-local.zip")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Empty(t, host)
	assert.Empty(t, remoteSnapshot)

	host, remoteSnapshot, ok, err = job.isBundleAvailable("local-bundle.zip")
	require.NoError(t, err)
	assert.False(t, ok, "bundles must mach bundle-*.zip pattern")
	assert.Empty(t, host)
	assert.Empty(t, remoteSnapshot)

	host, remoteSnapshot, ok, err = job.isBundleAvailable("bundle-123.zip")
	assert.False(t, ok)
	assert.Empty(t, host)
	assert.Empty(t, remoteSnapshot)
	require.NoError(t, err)
}

func TestCancelNotRunningJob(t *testing.T) {
	tools := &fakeDCOSTools{}
	dt := &Dt{
		Cfg:              testCfg(),
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools},
		MR:               &MonitoringResponse{},
	}
	router := NewRouter(dt)

	url := fmt.Sprintf("http://127.0.0.1:1050%s/report/diagnostics/status", baseRoute)
	mockedResponse := `
			{
			  "is_running":false,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}
	`
	st := &fakeDCOSTools{}
	st.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)
	dt.DtDCOSTools = st
	dt.DtDiagnosticsJob.DCOSTools = st

	// Job should fail because it is not running
	response, code, err := MakeHTTPRequest(t, router, "/system/health/v1/report/diagnostics/cancel", "POST", nil)
	assert.NoError(t, err)
	assert.Equal(t, code, http.StatusServiceUnavailable)
	var responseJSON diagnosticsReportResponse
	err = json.Unmarshal(response, &responseJSON)
	assert.NoError(t, err)
	assert.Equal(t, responseJSON, diagnosticsReportResponse{
		Version:      1,
		Status:       "Job is not running",
		ResponseCode: http.StatusServiceUnavailable,
	})
}

// Test we can cancel a job running on a different node.
func TestCancelGlobalJob(t *testing.T) {
	tools := &fakeDCOSTools{}
	dt := &Dt{
		Cfg:              testCfg(),
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools},
		MR:               &MonitoringResponse{},
	}
	router := NewRouter(dt)

	// mock job status response
	url := "http://127.0.0.1:1050/system/health/v1/report/diagnostics/status/all"
	mockedResponse := `{"10.0.7.252":{"is_running":false}}`

	mockedMasters := []dcos.Node{
		{
			Role: "master",
			IP:   "10.0.7.252",
		},
	}

	// add fake response for status/all
	st := &fakeDCOSTools{
		fakeMasters: mockedMasters,
	}
	st.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)

	// add fake response for status 10.0.7.252
	url = "http://10.0.7.252:1050/system/health/v1/report/diagnostics/status"
	mockedResponse = `
			{
			  "is_running":true,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}
	`
	st.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)
	dt.DtDCOSTools = st
	dt.DtDiagnosticsJob.DCOSTools = st

	MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/cancel", "POST", nil)

	// if we have the url in f.postRequestsMade, that means the redirect worked correctly
	assert.Contains(t, st.postRequestsMade, "http://10.0.7.252:1050/system/health/v1/report/diagnostics/cancel")
}

// try cancel a local job
func TestCancelLocalJob(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.TODO())
	tools := &fakeDCOSTools{}
	dt := &Dt{
		Cfg:              testCfg(),
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools, cancelFunc: cancelFunc},
		MR:               &MonitoringResponse{},
	}
	router := NewRouter(dt)

	dt.DtDiagnosticsJob.Running = true
	response, code, err := MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/cancel", "POST", nil)
	assert.NoError(t, err)
	assert.Equal(t, code, http.StatusOK)

	var responseJSON diagnosticsReportResponse
	err = json.Unmarshal(response, &responseJSON)
	require.NoError(t, err)
	assert.Equal(t, responseJSON, diagnosticsReportResponse{
		Version:      1,
		Status:       "Attempting to cancel a job, please check job status.",
		ResponseCode: http.StatusOK,
	})
	assert.Error(t, ctx.Err(), "context canceled")
}

func TestFailRunSnapshotJob(t *testing.T) {
	tools := &fakeDCOSTools{}
	dt := &Dt{
		Cfg:              testCfg(),
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools},
		MR:               &MonitoringResponse{},
	}
	router := NewRouter(dt)

	url := fmt.Sprintf("http://127.0.0.1:1050%s/report/diagnostics/status", baseRoute)
	mockedResponse := `
			{
			  "is_running":false,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_ended":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}
	`
	tools.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)

	// should fail since request is in wrong format
	body := bytes.NewBuffer([]byte(`{"nodes": "wrong"}`))
	_, code, _ := MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/create", "POST", body)
	assert.Equal(t, code, http.StatusBadRequest)

	// node should not be found
	body = bytes.NewBuffer([]byte(`{"nodes": ["192.168.0.1"]}`))
	response, code, _ := MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/create", "POST", body)
	assert.Equal(t, code, http.StatusServiceUnavailable)

	var responseJSON diagnosticsReportResponse
	err := json.Unmarshal(response, &responseJSON)
	require.NoError(t, err)
	assert.Equal(t, responseJSON.Status, "requested nodes: [192.168.0.1] not found")
}

func TestDeleteBundleWithInvalidName(t *testing.T) {
	tools := &fakeDCOSTools{}
	job := &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools}

	response, err := job.delete("invalid name")

	assert.EqualError(t, err, "format allowed  bundle-*.zip")
	assert.Equal(t, diagnosticsReportResponse{
		ResponseCode: 400,
		Status:       "format allowed  bundle-*.zip",
		Version:      1,
	}, response)
}

func TestDeleteBundleWhenBundleNotFound(t *testing.T) {
	tools := &fakeDCOSTools{}
	job := &DiagnosticsJob{Cfg: testCfg(), DCOSTools: tools}

	response, err := job.delete("bundle-test.zip")

	assert.NoError(t, err)
	assert.Equal(t, diagnosticsReportResponse{
		ResponseCode: 404,
		Status:       "Bundle not found bundle-test.zip",
		Version:      1,
	}, response)
}

func TestDeleteBundleWhenBundleExistOnLocalNode(t *testing.T) {
	tools := &fakeDCOSTools{}
	cfg := testCfg()
	defer os.RemoveAll(cfg.FlagDiagnosticsBundleDir)
	job := &DiagnosticsJob{Cfg: cfg, DCOSTools: tools}

	bundlePath := filepath.Join(cfg.FlagDiagnosticsBundleDir, "bundle-test.zip")
	f, err := os.Create(bundlePath)
	require.NoError(t, err)
	f.Close()
	require.NoError(t, err)

	response, err := job.delete("bundle-test.zip")

	assert.NoError(t, err)
	assert.Equal(t, diagnosticsReportResponse{
		ResponseCode: 200,
		Status:       "Deleted " + bundlePath,
		Version:      1,
	}, response)
}

func TestRunSnapshot(t *testing.T) {
	tools := &fakeDCOSTools{}
	cfg := testCfg()
	defer os.RemoveAll(cfg.FlagDiagnosticsBundleDir)
	dt := &Dt{
		Cfg:              cfg,
		DtDCOSTools:      tools,
		DtDiagnosticsJob: &DiagnosticsJob{Cfg: cfg, DCOSTools: tools},
		MR:               &MonitoringResponse{},
	}
	router := NewRouter(dt)

	url := "http://127.0.0.1:1050/system/health/v1/report/diagnostics/status"
	mockedResponse := `
			{
			  "is_running":false,
			  "status":"MyStatus",
			  "errors":null,
			  "last_bundle_dir":"/path/to/snapshot",
			  "job_started":"0001-01-01 00:00:00 +0000 UTC",
			  "job_ended":"0001-01-01 00:00:00 +0000 UTC",
			  "job_duration":"2s",
			  "diagnostics_bundle_dir":"/home/core/1",
			  "diagnostics_job_timeout_min":720,
			  "diagnostics_partition_disk_usage_percent":28.0,
			  "journald_logs_since_hours": "24",
			  "diagnostics_job_get_since_url_timeout_min": 5,
			  "command_exec_timeout_sec": 10
			}
	`
	tools.makeMockedResponse(url, []byte(mockedResponse), http.StatusOK, nil)
	// return empty list of endpoints
	tools.makeMockedResponse("http://127.0.0.1:1050/system/health/v1/logs", []byte("{}"), http.StatusOK, nil)

	body := bytes.NewBuffer([]byte(`{"nodes": ["all"]}`))
	response, code, _ := MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/create", "POST", body)
	assert.Equal(t, http.StatusOK, code)
	var responseJSON createResponse
	err := json.Unmarshal(response, &responseJSON)
	assert.NoError(t, err)

	bundleRegexp := `^bundle-[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{10}\.zip$`
	validBundleName := regexp.MustCompile(bundleRegexp)
	assert.True(t, validBundleName.MatchString(responseJSON.Extra.LastBundleFile),
		"invalid bundle name %s. Must match regexp: %s", responseJSON.Extra.LastBundleFile, bundleRegexp)

	assert.Equal(t, "Job has been successfully started", responseJSON.Status)
	assert.NotEmpty(t, responseJSON.Extra.LastBundleFile)

	assert.True(t, waitForBundle(t, router))
}

func waitForBundle(t *testing.T, router *mux.Router) bool {
	timeout := time.After(2 * time.Second)
	for {
		select {
		case <-timeout:
			t.Log("Timeout!")
			return false
		default:
			response, code, _ := MakeHTTPRequest(t, router, "http://127.0.0.1:1050/system/health/v1/report/diagnostics/status", "GET", nil)
			assert.Equal(t, http.StatusOK, code)
			var status bundleReportStatus
			err := json.Unmarshal(response, &status)
			assert.NoError(t, err)
			if !status.Running {
				return true
			}
		}
	}
}
