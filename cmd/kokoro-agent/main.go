package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Master  string `json:"master"`
	Token   string `json:"token"`
	AgentID string `json:"agent_id"`
	Alias   string `json:"alias"`
	Tags    string `json:"tags"`
}

type runtimeConfig struct {
	MetricsInterval time.Duration
	TCPPing         tcpPingConfig
}

type tcpPingConfig struct {
	Enabled     bool     `json:"enabled"`
	IntervalSec int      `json:"interval_sec"`
	Provinces   []string `json:"provinces"`
	EnableIPv4  bool     `json:"enable_ipv4"`
	EnableIPv6  bool     `json:"enable_ipv6"`
	Version     int      `json:"version"`
	UpdatedAt   int64    `json:"updated_at"`
}

type agent struct {
	cfg    Config
	rtMu   sync.RWMutex
	rt     runtimeConfig
	writeM sync.Mutex
}

type sysInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	Kernel   string `json:"kernel,omitempty"`
	Virt     string `json:"virt,omitempty"`
	CPUModel string `json:"cpu_model,omitempty"`
	Cores    int    `json:"cores,omitempty"`
}

type cpuTimes struct{ Idle, Total uint64 }
type netCounters struct{ Rx, Tx uint64 }
type diskInfo struct{ total, free, used uint64 }
type dialTarget struct {
	Address string
	Port    int
}

type wsConn struct {
	c      net.Conn
	reader *bufio.Reader
}

var carrierTargets4 = map[string]dialTarget{
	"telecom": {Address: "114.114.114.114", Port: 53},
	"mobile":  {Address: "221.179.155.161", Port: 53},
	"unicom":  {Address: "123.125.81.6", Port: 53},
}
var carrierTargets6 = map[string]dialTarget{
	"telecom": {Address: "240e:4c:4008::1", Port: 53},
	"mobile":  {Address: "2409:8088::a", Port: 53},
	"unicom":  {Address: "2408:8899::8", Port: 53},
}

func main() {
	var cfgPath, master, token, agentID, alias, tags string
	flag.StringVar(&cfgPath, "config", "/etc/kokoro-agent/config.json", "config path")
	flag.StringVar(&master, "master", "", "master ws/wss url")
	flag.StringVar(&token, "token", "", "agent token")
	flag.StringVar(&agentID, "agent-id", "", "agent id")
	flag.StringVar(&alias, "alias", "", "alias")
	flag.StringVar(&tags, "tags", "", "comma separated tags")
	flag.Parse()

	cfg, err := loadConfig(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fatal(err)
	}
	if master != "" {
		cfg.Master = master
	}
	if token != "" {
		cfg.Token = token
	}
	if agentID != "" {
		cfg.AgentID = agentID
	}
	if alias != "" {
		cfg.Alias = alias
	}
	if tags != "" {
		cfg.Tags = tags
	}
	if cfg.Master == "" || cfg.Token == "" || cfg.AgentID == "" {
		fatal(fmt.Errorf("master/token/agent-id required"))
	}
	if cfg.Alias == "" {
		h, _ := os.Hostname()
		cfg.Alias = h
	}

	a := &agent{cfg: cfg, rt: runtimeConfig{MetricsInterval: 5 * time.Second, TCPPing: tcpPingConfig{IntervalSec: 15, EnableIPv4: true, EnableIPv6: true}}}
	for {
		if err := a.run(); err != nil {
			fmt.Fprintf(os.Stderr, "agent loop error: %v\n", err)
		}
		time.Sleep(3 * time.Second)
	}
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	return cfg, json.Unmarshal(b, &cfg)
}

func (a *agent) run() error {
	conn, err := dialWebSocket(a.cfg.Master)
	if err != nil {
		return err
	}
	defer conn.Close()

	ipv4 := firstOutboundIP(4)
	ipv6 := firstOutboundIP(6)
	hello := map[string]any{
		"type":     "hello",
		"token":    a.cfg.Token,
		"agent_id": a.cfg.AgentID,
		"alias":    a.cfg.Alias,
		"tags":     parseTags(a.cfg.Tags),
		"ipv4":     ipv4,
		"ipv6":     ipv6,
		"ipv4_ok":  probeIPConnectivity(4),
		"ipv6_ok":  probeIPConnectivity(6),
		"sys":      collectSysInfo(),
	}
	if err := a.writeJSON(conn, hello); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.metricsLoop(ctx, conn)
	go a.tcpPingLoop(ctx, conn)

	for {
		msg, err := conn.ReadText()
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			continue
		}
		a.handleMessage(m)
	}
}

func (a *agent) handleMessage(m map[string]any) {
	if typ, _ := m["type"].(string); typ != "config" {
		return
	}
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	if v, ok := asInt(m["metrics_interval_ms"]); ok && v >= 1000 {
		a.rt.MetricsInterval = time.Duration(v) * time.Millisecond
	}
	if raw, ok := m["tcpping"]; ok {
		b, _ := json.Marshal(raw)
		var cfg tcpPingConfig
		if json.Unmarshal(b, &cfg) == nil {
			if cfg.IntervalSec <= 0 {
				cfg.IntervalSec = 15
			}
			if !cfg.EnableIPv4 && !cfg.EnableIPv6 {
				cfg.EnableIPv4 = true
			}
			a.rt.TCPPing = cfg
		}
	}
}

func (a *agent) metricsLoop(ctx context.Context, conn *wsConn) {
	prevCPU := readCPUTimes()
	prevNet := readNetCounters()
	for {
		interval := a.currentMetricsInterval()
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			metrics, cpuNow, netNow := collectMetrics(prevCPU, prevNet, interval)
			prevCPU, prevNet = cpuNow, netNow
			_ = a.writeJSON(conn, map[string]any{"type": "metrics", "metrics": metrics})
		}
	}
}

func (a *agent) tcpPingLoop(ctx context.Context, conn *wsConn) {
	for {
		cfg := a.currentTCPPing()
		wait := 5 * time.Second
		if cfg.Enabled && len(cfg.Provinces) > 0 {
			samples := runTCPPing(cfg)
			if len(samples) > 0 {
				_ = a.writeJSON(conn, map[string]any{"type": "tcpping_batch", "samples": samples})
			}
			wait = time.Duration(maxInt(cfg.IntervalSec, 5)) * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (a *agent) writeJSON(conn *wsConn, v any) error {
	a.writeM.Lock()
	defer a.writeM.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteText(b)
}

func (a *agent) currentMetricsInterval() time.Duration {
	a.rtMu.RLock()
	defer a.rtMu.RUnlock()
	if a.rt.MetricsInterval < time.Second {
		return 5 * time.Second
	}
	return a.rt.MetricsInterval
}
func (a *agent) currentTCPPing() tcpPingConfig {
	a.rtMu.RLock()
	defer a.rtMu.RUnlock()
	return a.rt.TCPPing
}

func dialWebSocket(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var c net.Conn
	if u.Scheme == "wss" {
		c, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", host, &tls.Config{ServerName: strings.Split(u.Host, ":")[0]})
	} else {
		c, err = net.DialTimeout("tcp", host, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		c.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nUser-Agent: kokoro-agent/1\r\n\r\n", path, u.Host, key)
	if _, err := io.WriteString(c, req); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		c.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", resp.Status)
	}
	accept := resp.Header.Get("Sec-WebSocket-Accept")
	want := wsAccept(key)
	if accept != want {
		c.Close()
		return nil, fmt.Errorf("websocket bad accept")
	}
	return &wsConn{c: c, reader: br}, nil
}

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func (w *wsConn) Close() error { return w.c.Close() }

func (w *wsConn) WriteText(payload []byte) error {
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x81)
	maskBit := byte(0x80)
	n := len(payload)
	switch {
	case n <= 125:
		frame = append(frame, maskBit|byte(n))
	case n <= 65535:
		frame = append(frame, maskBit|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, maskBit|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	frame = append(frame, masked...)
	_, err := w.c.Write(frame)
	return err
}

func (w *wsConn) ReadText() ([]byte, error) {
	for {
		h1, err := w.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		h2, err := w.reader.ReadByte()
		if err != nil {
			return nil, err
		}
		opcode := h1 & 0x0f
		masked := (h2 & 0x80) != 0
		ln := int64(h2 & 0x7f)
		switch ln {
		case 126:
			var x uint16
			if err := binary.Read(w.reader, binary.BigEndian, &x); err != nil {
				return nil, err
			}
			ln = int64(x)
		case 127:
			var x uint64
			if err := binary.Read(w.reader, binary.BigEndian, &x); err != nil {
				return nil, err
			}
			ln = int64(x)
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(w.reader, maskKey[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, ln)
		if _, err := io.ReadFull(w.reader, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch opcode {
		case 0x1:
			return payload, nil
		case 0x8:
			return nil, io.EOF
		case 0x9:
			_ = w.writeControl(0xA, payload)
		case 0xA:
			continue
		default:
			continue
		}
	}
}

func (w *wsConn) writeControl(opcode byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	frame := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.c.Write(frame)
	return err
}

func runTCPPing(cfg tcpPingConfig) []map[string]any {
	now := time.Now().Unix()
	out := make([]map[string]any, 0)
	for _, prov := range cfg.Provinces {
		prov = strings.TrimSpace(prov)
		if prov == "" {
			continue
		}
		if cfg.EnableIPv4 {
			for carrier, target := range carrierTargets4 {
				rtt, ok := tcpPing("tcp4", net.JoinHostPort(target.Address, strconv.Itoa(target.Port)), 2*time.Second)
				out = append(out, map[string]any{"ts": now, "province": prov, "carrier": carrier, "ipver": 4, "rtt_ms": rtt, "ok": boolToInt(ok)})
			}
		}
		if cfg.EnableIPv6 {
			for carrier, target := range carrierTargets6 {
				rtt, ok := tcpPing("tcp6", net.JoinHostPort(target.Address, strconv.Itoa(target.Port)), 2*time.Second)
				out = append(out, map[string]any{"ts": now, "province": prov, "carrier": carrier, "ipver": 6, "rtt_ms": rtt, "ok": boolToInt(ok)})
			}
		}
	}
	return out
}

func tcpPing(network, addr string, timeout time.Duration) (float64, bool) {
	start := time.Now()
	c, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return float64(timeout.Milliseconds()), false
	}
	_ = c.Close()
	rtt := time.Since(start).Seconds() * 1000
	return math.Round(rtt*100) / 100, true
}

func collectSysInfo() sysInfo {
	host, _ := os.Hostname()
	osName := readDistroName()
	if osName == "" {
		osName = runtime.GOOS
	}
	return sysInfo{
		OS:       osName,
		Arch:     runtime.GOARCH,
		Hostname: host,
		Kernel:   readFirstLine("/proc/sys/kernel/osrelease"),
		Virt:     detectVirt(),
		CPUModel: readCPUModel(),
		Cores:    runtime.NumCPU(),
	}
}

func probeIPConnectivity(family int) bool {
	if family == 6 {
		c, err := net.DialTimeout("tcp6", "[2606:4700:4700::1111]:53", 2*time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	c, err := net.DialTimeout("tcp4", "1.1.1.1:53", 2*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(strings.ToLower(line), "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func readDistroName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		vals[parts[0]] = strings.Trim(parts[1], "\"'")
	}
	name := vals["ID"]
	ver := vals["VERSION_ID"]
	pretty := vals["PRETTY_NAME"]
	if name != "" && ver != "" {
		compact := strings.ReplaceAll(ver, ".", "")
		return strings.ToLower(name) + compact
	}
	if pretty != "" {
		return pretty
	}
	return runtime.GOOS
}

func detectVirt() string {
	if out, err := exec.Command("systemd-detect-virt", "--quiet", "--container").CombinedOutput(); err == nil || len(out) > 0 {
		if len(out) > 0 {
			return strings.ToUpper(strings.TrimSpace(string(out)))
		}
		return "container"
	}
	if out, err := exec.Command("systemd-detect-virt").Output(); err == nil {
		v := strings.TrimSpace(string(out))
		if v != "" && v != "none" {
			v = strings.ToUpper(v)
			if v == "QEMU" {
				return "KVM"
			}
			return v
		}
	}
	// fallback: common KVM/QEMU hints
	for _, path := range []string{"/sys/devices/virtual/dmi/id/product_name", "/sys/class/dmi/id/product_name"} {
		if b, err := os.ReadFile(path); err == nil {
			v := strings.ToUpper(strings.TrimSpace(string(b)))
			if strings.Contains(v, "KVM") || strings.Contains(v, "QEMU") {
				return "KVM"
			}
			if strings.Contains(v, "VMWARE") {
				return "VMWARE"
			}
			if strings.Contains(v, "VIRTUALBOX") {
				return "VBOX"
			}
			if strings.Contains(v, "HVM DOMU") || strings.Contains(v, "XEN") {
				return "XEN"
			}
			if strings.Contains(v, "MICROSOFT") {
				return "HYPERV"
			}
		}
	}
	return ""
}

func collectMetrics(prevCPU cpuTimes, prevNet netCounters, interval time.Duration) (map[string]any, cpuTimes, netCounters) {
	cpuNow := readCPUTimes()
	netNow := readNetCounters()
	mem := readMemInfo()
	disk := readDiskUsage("/")
	load1, load5, load15 := readLoadAvg()
	cpuPct := cpuPercent(prevCPU, cpuNow)
	netRxRate, netTxRate := 0.0, 0.0
	if interval > 0 {
		netRxRate = float64(diffU64(netNow.Rx, prevNet.Rx)) / interval.Seconds()
		netTxRate = float64(diffU64(netNow.Tx, prevNet.Tx)) / interval.Seconds()
	}
	return map[string]any{
		"ts":      time.Now().Unix(),
		"uptime":  readUptimeSeconds(),
		"cpu":     map[string]any{"used_percent": cpuPct, "cores": runtime.NumCPU(), "load1": load1, "load5": load5, "load15": load15},
		"memory":  map[string]any{"total": mem["MemTotal"], "available": mem["MemAvailable"], "used": maxU64(mem["MemTotal"]-mem["MemAvailable"], 0), "swap_total": mem["SwapTotal"], "swap_free": mem["SwapFree"]},
		"disk":    map[string]any{"total": disk.total, "free": disk.free, "used": disk.used},
		"network": map[string]any{"rx_bytes": netNow.Rx, "tx_bytes": netNow.Tx, "rx_rate": netRxRate, "tx_rate": netTxRate},
	}, cpuNow, netNow
}

func readDiskUsage(path string) diskInfo {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskInfo{}
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return diskInfo{total: total, free: free, used: total - free}
}

func readLoadAvg() (float64, float64, float64) {
	parts := strings.Fields(readFirstLine("/proc/loadavg"))
	if len(parts) < 3 {
		return 0, 0, 0
	}
	return atof(parts[0]), atof(parts[1]), atof(parts[2])
}
func readUptimeSeconds() int64 {
	parts := strings.Fields(readFirstLine("/proc/uptime"))
	if len(parts) == 0 {
		return 0
	}
	return int64(atof(parts[0]))
}
func readMemInfo() map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.SplitN(s.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) == 0 {
			continue
		}
		v, _ := strconv.ParseUint(fields[0], 10, 64)
		out[key] = v * 1024
	}
	if _, ok := out["MemAvailable"]; !ok {
		out["MemAvailable"] = out["MemFree"] + out["Buffers"] + out["Cached"]
	}
	return out
}
func readCPUTimes() cpuTimes {
	fields := strings.Fields(readFirstLine("/proc/stat"))
	if len(fields) < 5 {
		return cpuTimes{}
	}
	vals := make([]uint64, 0, len(fields)-1)
	for _, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		vals = append(vals, v)
	}
	var total uint64
	for _, v := range vals {
		total += v
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4]
	}
	return cpuTimes{Idle: idle, Total: total}
}
func cpuPercent(prev, curr cpuTimes) float64 {
	totald := diffU64(curr.Total, prev.Total)
	idled := diffU64(curr.Idle, prev.Idle)
	if totald == 0 {
		return 0
	}
	used := 100 * (1 - float64(idled)/float64(totald))
	return math.Round(used*100) / 100
}
func readNetCounters() netCounters {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return netCounters{}
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	var out netCounters
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		out.Rx += rx
		out.Tx += tx
	}
	return out
}
func parseTags(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
func readFirstLine(path string) string {
	b, _ := os.ReadFile(filepath.Clean(path))
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}
func firstOutboundIP(ver int) string {
	network, target := "udp4", "8.8.8.8:80"
	if ver == 6 {
		network, target = "udp6", "[2001:4860:4860::8888]:80"
	}
	c, err := net.Dial(network, target)
	if err != nil {
		return ""
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return ""
}
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
func diffU64(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return 0
}
func atof(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case int64:
		return int(x), true
	default:
		return 0, false
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
