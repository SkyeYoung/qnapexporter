package prometheus

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ping/ping"
	"github.com/mackerelio/go-osstat/loadavg"
	"github.com/mackerelio/go-osstat/uptime"
	nut "github.com/robbiet480/go.nut"

	"gitlab.com/pedropombeiro/qnapexporter/lib/exporter"
	"gitlab.com/pedropombeiro/qnapexporter/lib/utils"
)

const (
	devDir              = "/dev"
	netDir              = "/sys/class/net"
	flashcacheStatsPath = "/proc/flashcache/CG0/flashcache_stats"

	envValidity    = time.Duration(5 * time.Minute)
	volumeValidity = time.Duration(1 * time.Minute)
)

type promExporter struct {
	logger *log.Logger

	hostname   string
	pingTarget string

	upsClient       nut.Client
	upsConnErr      error
	upsConnAttempts int
	upsList         *[]nut.UPS
	upsLock         sync.Mutex

	getsysinfo string
	syshdnum   int
	sysfannum  int
	ifaces     []string
	iostat     string
	devices    []string
	envExpiry  time.Time

	volumes      []volumeInfo
	volumeExpiry time.Time

	fns []func() ([]metric, error)
}

func NewExporter(pingTarget string, logger *log.Logger) exporter.Exporter {
	e := &promExporter{
		logger:       logger,
		pingTarget:   pingTarget,
		volumeExpiry: time.Now(),
		envExpiry:    time.Now(),
	}
	e.fns = []func() ([]metric, error){
		getUptimeMetrics,          // #1
		getLoadAvgMetrics,         // #2
		getCpuRatioMetrics,        // #3
		getMemInfoMetrics,         // #4
		e.getUpsStatsMetrics,      // #5
		e.getSysInfoTempMetrics,   // #6
		e.getSysInfoFanMetrics,    // #7
		e.getSysInfoHdMetrics,     // #8
		e.getSysInfoVolMetrics,    // #9
		e.getDiskStatsMetrics,     // #10
		getFlashCacheStatsMetrics, // #11
		e.getNetworkStatsMetrics,  // #12
		e.getPingMetrics,          // #13
	}

	return e
}

func (e *promExporter) WriteMetrics(w io.Writer) error {
	if time.Now().After(e.envExpiry) {
		e.readEnvironment()
	}

	var wg sync.WaitGroup
	metricsCh := make(chan interface{}, 4)
	for idx, fn := range e.fns {
		wg.Add(1)

		go fetchMetricsWorker(&wg, metricsCh, idx, fn)
	}

	go func() {
		// Close channel once all workers are done
		wg.Wait()
		close(metricsCh)
	}()

	// Retrieve metrics from channel and write them to the response
	for m := range metricsCh {
		switch v := m.(type) {
		case []metric:
			for _, m := range v {
				writeMetricMetadata(w, m)
				_, _ = fmt.Fprintf(w, "%s %g\n", e.getMetricFullName(m), m.value)
			}
		case error:
			e.logger.Println(v.Error())

			_, _ = fmt.Fprintf(w, "## %v\n", v)
		}
	}

	return nil
}

func fetchMetricsWorker(wg *sync.WaitGroup, metricsCh chan<- interface{}, idx int, fetchMetricsFn func() ([]metric, error)) {
	defer wg.Done()

	metrics, err := fetchMetricsFn()
	if err != nil {
		metricsCh <- fmt.Errorf("retrieve metric #%d: %w", 1+idx, err)
		return
	}

	metricsCh <- metrics
}

func (e *promExporter) Close() {
	if e.upsClient.ProtocolVersion != "" {
		e.upsLock.Lock()
		_, _ = e.upsClient.Disconnect()
		e.upsLock.Unlock()
	}
}

func (e *promExporter) readEnvironment() {
	e.logger.Println("Reading environment...")

	var err error
	e.hostname = os.Getenv("HOSTNAME")
	if e.hostname == "" {
		e.hostname, err = utils.ExecCommand("hostname")
	}
	e.logger.Printf("Hostname: %s, err=%v\n", e.hostname, err)

	if e.iostat == "" {
		e.iostat, err = exec.LookPath("iostat")
		if err != nil {
			e.logger.Printf("Failed to find iostat: %v\n", err)
		}
	}
	if e.getsysinfo == "" {
		e.getsysinfo, _ = exec.LookPath("getsysinfo")
		if err != nil {
			e.logger.Printf("Failed to find getsysinfo: %v\n", err)
		}
	}
	if e.getsysinfo != "" {
		hdnumOutput, err := utils.ExecCommand(e.getsysinfo, "hdnum")
		if err == nil {
			e.syshdnum, _ = strconv.Atoi(hdnumOutput)
		} else {
			e.syshdnum = -1
		}

		sysfannumOutput, err := utils.ExecCommand(e.getsysinfo, "sysfannum")
		if err == nil {
			e.sysfannum, _ = strconv.Atoi(sysfannumOutput)
		} else {
			e.sysfannum = -1
		}

		e.readSysVolInfo()
	}

	info, _ := ioutil.ReadDir(netDir)
	e.ifaces = make([]string, 0, len(info))
	for _, d := range info {
		iface := d.Name()
		if !strings.HasPrefix(iface, "eth") {
			continue
		}

		e.ifaces = append(e.ifaces, iface)
	}

	info, _ = ioutil.ReadDir(devDir)
	e.devices = make([]string, 0, len(info))
	for _, d := range info {
		dev := d.Name()
		if d.IsDir() || !strings.HasPrefix(dev, "nvme") && !strings.HasPrefix(dev, "sd") {
			continue
		}
		switch {
		case strings.HasPrefix(dev, "nvme") && len(dev) != 7:
			continue
		case strings.HasPrefix(dev, "sd") && len(dev) != 3:
			continue
		}

		e.devices = append(e.devices, dev)
	}
	e.logger.Printf("Found devices: %v", e.devices)

	e.envExpiry = e.envExpiry.Add(envValidity)
}

func (e *promExporter) getMetricFullName(m metric) string {
	if m.attr != "" {
		return fmt.Sprintf(`%s{node=%q,%s}`, m.name, e.hostname, m.attr)
	}

	return fmt.Sprintf(`%s{node=%q}`, m.name, e.hostname)
}

func writeMetricMetadata(w io.Writer, m metric) {
	if m.help != "" {
		fmt.Fprintln(w, "# HELP "+m.name+" "+m.help)
	}
	if m.metricType != "" {
		fmt.Fprintln(w, "# TYPE "+m.name+" "+m.metricType)
	}
}

func getUptimeMetrics() ([]metric, error) {
	u, err := uptime.Get()
	if err != nil {
		return nil, err
	}

	return []metric{
		{
			name:       "node_time_seconds",
			value:      u.Seconds(),
			help:       "System uptime measured in seconds",
			metricType: "counter",
		},
	}, err
}

func getLoadAvgMetrics() ([]metric, error) {
	s, err := loadavg.Get()
	if err != nil {
		return nil, err
	}

	metrics := []metric{
		{name: "node_load1", value: s.Loadavg1},
		{name: "node_load5", value: s.Loadavg5},
		{name: "node_load15", value: s.Loadavg15},
	}
	return metrics, nil
}

func (e *promExporter) getSysInfoTempMetrics() ([]metric, error) {
	if e.getsysinfo == "" {
		return nil, nil
	}

	metrics := make([]metric, 0, 2)

	for _, dev := range []string{"cputmp", "systmp"} {
		output, err := utils.ExecCommand(e.getsysinfo, dev)
		if err != nil {
			return nil, err
		}

		tokens := strings.SplitN(output, " ", 2)
		value, err := strconv.ParseFloat(tokens[0], 64)
		if err != nil {
			continue
		}

		metrics = append(metrics, metric{
			name:  fmt.Sprintf("node_%s_C", dev),
			value: value,
		})
	}

	return metrics, nil
}

func (e *promExporter) getSysInfoHdMetrics() ([]metric, error) {
	if e.getsysinfo == "" {
		return nil, nil
	}

	metrics := make([]metric, 0, e.syshdnum)
	highestAvailable := 0

	for hdnum := 1; hdnum <= e.syshdnum; hdnum++ {
		hdnumStr := strconv.Itoa(hdnum)
		tempStr, err := utils.ExecCommand(e.getsysinfo, "hdtmp", hdnumStr)
		if err != nil {
			return nil, err
		}
		if tempStr == "--" {
			continue
		}

		smart, err := utils.ExecCommand(e.getsysinfo, "hdsmart", hdnumStr)
		if err != nil {
			return nil, err
		}

		temp, err := strconv.ParseFloat(strings.SplitN(tempStr, " ", 2)[0], 64)
		if err != nil {
			return metrics, err
		}

		metrics = append(metrics, metric{
			name:  "node_hdtmp_C",
			attr:  fmt.Sprintf(`hd=%q,smart=%q`, hdnumStr, smart),
			value: temp,
		})
		highestAvailable = hdnum
	}

	// Do not ask for data next time on disks that do not report it
	e.syshdnum = highestAvailable

	return metrics, nil
}

func (e *promExporter) getSysInfoFanMetrics() ([]metric, error) {
	if e.getsysinfo == "" {
		return nil, nil
	}

	metrics := make([]metric, 0, e.sysfannum)

	for fannum := 1; fannum <= e.sysfannum; fannum++ {
		fannumStr := strconv.Itoa(fannum)

		fanStr, err := utils.ExecCommand(e.getsysinfo, "sysfan", fannumStr)
		if err != nil {
			return nil, err
		}

		fan, err := strconv.ParseFloat(strings.SplitN(fanStr, " ", 2)[0], 64)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, metric{
			name:  "node_sysfan_RPM",
			attr:  fmt.Sprintf(`fan=%q`, fannumStr),
			value: fan,
		})
	}

	return metrics, nil
}

func getFlashCacheStatsMetrics() ([]metric, error) {
	lines, err := utils.ReadFileLines(flashcacheStatsPath)
	if err != nil {
		return nil, err
	}

	metrics := make([]metric, 0, len(lines))
	for _, line := range lines {
		tokens := strings.SplitN(line, ":", 2)
		valueStr := strings.TrimSpace(tokens[1])
		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			return nil, err
		}

		metrics = append(metrics, metric{
			name:  "node_flashcache_" + tokens[0],
			value: value,
		})
	}

	return metrics, nil
}

func (e *promExporter) getNetworkStatsMetrics() ([]metric, error) {
	metrics := make([]metric, 0, len(e.ifaces)*2)
	for _, iface := range e.ifaces {
		rxMetric, err := getNetworkStatMetric("node_network_receive_bytes_total", "Total number of bytes received", iface, "rx")
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, rxMetric)

		txMetric, err := getNetworkStatMetric("node_network_transmit_bytes_total", "Total number of bytes transmitted", iface, "tx")
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, txMetric)
	}

	return metrics, nil
}

func getNetworkStatMetric(name string, help string, iface string, direction string) (metric, error) {
	str, err := utils.ReadFile(path.Join(netDir, iface, "statistics", direction+"_bytes"))
	if err != nil {
		return metric{}, err
	}

	value, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return metric{}, err
	}

	return metric{
		name:       name,
		attr:       fmt.Sprintf(`device=%q`, iface),
		value:      value,
		help:       help,
		metricType: "counter",
	}, nil
}

func (e *promExporter) getDiskStatsMetrics() ([]metric, error) {
	if e.iostat == "" {
		return nil, nil
	}

	args := []string{"-k", "-d"}
	args = append(args, e.devices...)
	lines, err := utils.ExecCommandGetLines(e.iostat, args...)
	if err != nil {
		return nil, err
	}

	if len(lines) < 4 {
		return nil, fmt.Errorf("iostat output missing expected lines - found %d lines", len(lines))
	}

	metrics := make([]metric, 0, len(e.devices)*2)
	for _, line := range lines[3:] {
		readMetric, err := e.getDiskStatMetric("node_disk_read_kbytes_total", "Total number of kilobytes read", line, 5)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, readMetric)

		writeMetric, err := e.getDiskStatMetric("node_disk_written_kbytes_total", "Total number of kilobytes written", line, 6)
		if err != nil {
			return nil, err
		}
		metrics = append(metrics, writeMetric)
	}

	return metrics, nil
}

func (e *promExporter) getDiskStatMetric(name string, help string, line string, field int) (metric, error) {
	fields := strings.Fields(line)
	if field >= len(fields) {
		return metric{}, fmt.Errorf("disk stat metric %q: field %d missing in %d total fields", name, field, len(fields))
	}

	value, err := strconv.ParseFloat(fields[field], 64)
	if err != nil {
		return metric{}, err
	}

	dev := fields[0]
	return metric{
		name:       name,
		attr:       fmt.Sprintf(`device=%q`, dev),
		value:      value,
		help:       help,
		metricType: "counter",
	}, nil
}

func (e *promExporter) getPingMetrics() ([]metric, error) {
	pinger, err := ping.NewPinger(e.pingTarget)
	if err != nil {
		return nil, err
	}

	pinger.SetPrivileged(true)
	pinger.Timeout = 2 * time.Second
	pinger.Count = 1
	err = pinger.Run() // Blocks until finished.
	if err != nil {
		return nil, err
	}

	stats := pinger.Statistics() // get send/receive/rtt stats
	value := float64(stats.AvgRtt.Seconds()) * 1000.0
	if stats.PacketLoss > 0 {
		value = math.NaN()
	}
	m := metric{
		name:  "node_network_external_roundtrip_time_ms",
		attr:  fmt.Sprintf("target=%q", pinger.IPAddr().String()),
		value: value,
	}

	return []metric{m}, nil
}
