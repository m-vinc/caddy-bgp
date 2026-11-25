//go:build lab

package lab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	anycast4 = "198.51.100.100"
	anycast6 = "2001:db8:ffff::100"
	logFile  = "/tmp/caddy.log"
)

var (
	routers   = []string{"rt1", "rt2"}
	addresses = map[string][]string{
		"lb1": {"192.0.2.11", "2001:db8:1::11"},
		"lb2": {"192.0.2.12", "2001:db8:1::12"},
	}
)

type nextHop struct {
	IP string `json:"ip"`
}

type bgpPath struct {
	Valid     bool      `json:"valid"`
	Multipath bool      `json:"multipath"`
	NextHops  []nextHop `json:"nexthops"`
	Community community `json:"community"`
	Med       int       `json:"metric"`
}

type community struct {
	Text string `json:"string"`
}

type bgpRoute struct {
	Paths []bgpPath `json:"paths"`
}

type neighbor struct {
	State       string `json:"bgpState"`
	Established int    `json:"connectionsEstablished"`
	Dropped     int    `json:"connectionsDropped"`
	ResetReason string `json:"lastResetDueTo"`
}

type peerStatus struct {
	Address     string   `json:"address"`
	Established bool     `json:"established"`
	Received    int      `json:"received_routes"`
	Imported    int      `json:"imported_routes"`
	Advertised  []string `json:"advertised"`
	BFD         string   `json:"bfd_state"`
	LastDown    string   `json:"last_down_reason"`
}

func command(arguments ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	output, err := exec.CommandContext(ctx, "docker", append([]string{"compose"}, arguments...)...).CombinedOutput()
	return string(output), err
}

func compose(t *testing.T, arguments ...string) string {
	t.Helper()

	output, err := command(arguments...)
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}

	return output
}

func shell(t *testing.T, service, script string) string {
	t.Helper()

	return compose(t, "exec", "-T", service, "sh", "-c", script)
}

func vtysh(t *testing.T, router, query string, target any) {
	t.Helper()

	output := compose(t, "exec", "-T", router, "vtysh", "-c", query)
	if err := json.Unmarshal([]byte(output), target); err != nil {
		t.Fatalf("%s: %s: %v\n%s", router, query, err, output)
	}
}

func startCaddy(t *testing.T, node string) {
	t.Helper()

	run := "exec caddy run --config /etc/caddy/Caddyfile >>" + logFile + " 2>&1"
	if node == "lb1" {
		compose(t, "exec", "-d", node, "sh", "-c", run)
		return
	}

	compose(t, "exec", "-d", node, "su", "-s", "/bin/sh", "nobody", "-c", "export HOME=/tmp; "+run)
}

func signal(t *testing.T, node, name string) {
	t.Helper()

	shell(t, node, "kill -"+name+" $(pgrep -x caddy)")
}

func running(node string) bool {
	_, err := command("exec", "-T", node, "pgrep", "-x", "caddy")
	return err == nil
}

func healthy(t *testing.T, node string, value bool) {
	t.Helper()

	if value {
		shell(t, node, "touch /tmp/healthy && chmod 644 /tmp/healthy")
		return
	}

	shell(t, node, "rm -f /tmp/healthy")
}

func paths(t *testing.T, router, address string) []bgpPath {
	t.Helper()

	query := "show bgp ipv4 unicast " + address + "/32 json"
	if strings.Contains(address, ":") {
		query = "show bgp ipv6 unicast " + address + "/128 json"
	}

	var route bgpRoute
	vtysh(t, router, query, &route)
	return route.Paths
}

func holders(t *testing.T, router, address string) []string {
	t.Helper()

	var nodes []string
	for _, path := range paths(t, router, address) {
		for node, own := range addresses {
			if len(path.NextHops) > 0 && slices.Contains(own, path.NextHops[0].IP) && path.Valid {
				nodes = append(nodes, node)
			}
		}
	}

	slices.Sort(nodes)
	return nodes
}

func everywhere(t *testing.T, wanted ...string) bool {
	t.Helper()

	for _, router := range routers {
		if !slices.Equal(holders(t, router, anycast4), wanted) || !slices.Equal(holders(t, router, anycast6), wanted) {
			return false
		}
	}

	return true
}

func eventually(t *testing.T, limit time.Duration, what string, condition func() bool) time.Duration {
	t.Helper()

	started := time.Now()
	for time.Since(started) < limit {
		if condition() {
			elapsed := time.Since(started)
			t.Logf("%s after %s", what, elapsed.Truncate(100*time.Millisecond))
			return elapsed
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("timed out after %s waiting for: %s\n%s", limit, what, dump(t))
	return 0
}

func dump(t *testing.T) string {
	t.Helper()

	var report strings.Builder
	for _, router := range routers {
		for _, query := range []string{"show bgp ipv4 unicast", "show bgp ipv6 unicast", "show bgp summary"} {
			_, _ = report.WriteString(compose(t, "exec", "-T", router, "vtysh", "-c", query))
		}
	}

	for node := range addresses {
		_, _ = report.WriteString(node + ":\n" + shell(t, node, "tail -25 "+logFile))
	}

	return report.String()
}

func neighbors(t *testing.T, router, node string) []neighbor {
	t.Helper()

	var result []neighbor
	for _, address := range addresses[node] {
		entries := make(map[string]neighbor)
		vtysh(t, router, "show bgp neighbors "+address+" json", &entries)
		result = append(result, entries[address])
	}

	return result
}

func sessions(t *testing.T, node string) []int {
	t.Helper()

	var counts []int
	for _, router := range routers {
		for _, entry := range neighbors(t, router, node) {
			counts = append(counts, entry.Established, entry.Dropped)
		}
	}

	return counts
}

func peers(t *testing.T, node string) []peerStatus {
	t.Helper()

	var result []peerStatus
	output := shell(t, node, "wget -qO- http://127.0.0.1:2019/bgp/peers")
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("%v: %s", err, output)
	}

	return result
}

func requests(t *testing.T, target string, count int) (map[string]int, int) {
	t.Helper()

	return fetch(t, fmt.Sprintf("for i in $(seq %d); do wget -T 1 -qO- 'http://%s/' || echo failed; echo; done", count, target))
}

func fetch(t *testing.T, script string) (map[string]int, int) {
	t.Helper()

	return tally(shell(t, "client", script))
}

func tally(output string) (map[string]int, int) {
	seen := make(map[string]int)
	failures := 0
	for line := range strings.Lines(output) {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "lb1" || fields[0] == "lb2":
			seen[fields[0]]++
		default:
			failures++
		}
	}

	return seen, failures
}

func ready(t *testing.T) {
	t.Helper()

	for node := range addresses {
		healthy(t, node, true)
		if !running(node) {
			startCaddy(t, node)
		}
	}

	eventually(t, time.Minute, "both nodes announced on both routers", func() bool { return everywhere(t, "lb1", "lb2") })
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	defer func() { _, _ = command("down", "-v", "-t", "1") }()

	if output, err := command("up", "-d", "--wait", "--wait-timeout", "60"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, output)
		return 1
	}

	script := "ip link add cbgp0 type dummy && ip link set cbgp0 up && ip addr add " + anycast4 + "/32 dev cbgp0 && ip -6 addr add " + anycast6 + "/128 dev cbgp0"
	steps := [][]string{
		{"exec", "-T", "lb2", "sh", "-c", script},
		{"exec", "-T", "client", "sh", "-c", "ip route add " + anycast4 + "/32 via 192.0.2.1 && ip -6 route add " + anycast6 + "/128 via 2001:db8:1::1"},
	}
	for _, step := range steps {
		if output, err := command(step...); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, output)
			return 1
		}
	}

	return m.Run()
}

func TestAnnounceWithECMP(t *testing.T) {
	ready(t)
	for _, address := range []string{anycast4, anycast6} {
		for _, router := range routers {
			for _, path := range paths(t, router, address) {
				if !path.Multipath || path.Community.Text != "65000:100" || path.Med != 50 {
					t.Errorf("%s: %s: %+v", router, address, path)
				}
			}

			family := "-4"
			if strings.Contains(address, ":") {
				family = "-6"
			}

			kernel := compose(t, "exec", "-T", router, "ip", family, "route", "show", address)
			if strings.Count(kernel, "nexthop") != 2 {
				t.Errorf("%s: kernel route %s", router, kernel)
			}
		}
	}

	for _, target := range []string{anycast4, "[" + anycast6 + "]"} {
		seen, failures := requests(t, target, 60)
		t.Logf("%s: %v, %d failures", target, seen, failures)
		if failures > 0 || seen["lb1"] == 0 || seen["lb2"] == 0 {
			t.Errorf("%s: %v, %d failures", target, seen, failures)
		}
	}
}

func TestPolicies(t *testing.T) {
	ready(t)
	for node := range addresses {
		for _, peer := range peers(t, node) {
			if !peer.Established || peer.Received == 0 || peer.Imported != 0 || len(peer.Advertised) != 1 {
				t.Errorf("%s: %+v", node, peer)
			}

			if peer.Address == "192.0.2.2" && peer.BFD != "up" {
				t.Errorf("%s: bfd %q", node, peer.BFD)
			}
		}

		table := shell(t, node, "ip route show table all; ip -6 route show table all")
		if strings.Contains(table, "203.0.113.0") || strings.Contains(table, "2001:db8:dead") || strings.Contains(table, "proto bgp") {
			t.Errorf("%s installed learned routes:\n%s", node, table)
		}
	}

	capabilities := shell(t, "lb2", "grep CapEff /proc/$(pgrep -x caddy)/status")
	if strings.Fields(capabilities)[1] != "0000000000000000" {
		t.Errorf("lb2 runs with %s", capabilities)
	}
}

func TestUnhealthyWithdrawsWithoutReset(t *testing.T) {
	ready(t)
	before := sessions(t, "lb1")
	healthy(t, "lb1", false)
	eventually(t, 10*time.Second, "withdrawal of the unhealthy node", func() bool { return everywhere(t, "lb2") })
	if seen, failures := requests(t, anycast4, 20); seen["lb2"] != 20 || failures > 0 {
		t.Errorf("%v, %d failures", seen, failures)
	}

	healthy(t, "lb1", true)
	eventually(t, 20*time.Second, "return of the healthy node", func() bool { return everywhere(t, "lb1", "lb2") })
	if after := sessions(t, "lb1"); !slices.Equal(before, after) {
		t.Errorf("sessions %v then %v", before, after)
	}
}

func TestReloadKeepsSessions(t *testing.T) {
	ready(t)
	before := sessions(t, "lb1")
	shell(t, "lb1", "cd /etc/caddy && GREETING=reloaded caddy reload --force 2>&1")
	if answer := shell(t, "lb1", "wget -qO- http://127.0.0.1/"); !strings.Contains(answer, "reloaded") {
		t.Errorf("the reload did not apply: %s", answer)
	}

	shell(t, "lb1", "cd /etc/caddy && NODE_MED=10 caddy reload 2>&1")
	eventually(t, 5*time.Second, "new MED", func() bool { return hasMED(t, 10) })
	if output, err := command("exec", "-T", "lb1", "sh", "-c", "cd /etc/caddy && NODE_MED=many caddy reload 2>&1"); err == nil {
		t.Errorf("an invalid reload succeeded: %s", output)
	}

	if !hasMED(t, 10) {
		t.Error("the failed reload changed the MED")
	}

	shell(t, "lb1", "cd /etc/caddy && caddy reload 2>&1")
	eventually(t, 5*time.Second, "previous MED", func() bool { return everywhere(t, "lb1", "lb2") && !hasMED(t, 10) })
	if after := sessions(t, "lb1"); !slices.Equal(before, after) {
		t.Errorf("sessions %v then %v", before, after)
	}
}

func hasMED(t *testing.T, med int) bool {
	t.Helper()

	for _, path := range paths(t, "rt1", anycast4) {
		if path.Med == med && path.NextHops[0].IP == addresses["lb1"][0] {
			return true
		}
	}

	return false
}

func TestDrainThroughAdmin(t *testing.T) {
	ready(t)
	before := sessions(t, "lb1")
	shell(t, "lb1", "wget -qO- --post-data= 'http://127.0.0.1:2019/bgp/drain'")
	eventually(t, 5*time.Second, "drain", func() bool { return everywhere(t, "lb2") })
	shell(t, "lb1", "wget -qO- --post-data= 'http://127.0.0.1:2019/bgp/undrain'")
	eventually(t, 5*time.Second, "undrain", func() bool { return everywhere(t, "lb1", "lb2") })
	if after := sessions(t, "lb1"); !slices.Equal(before, after) {
		t.Errorf("sessions %v then %v", before, after)
	}
}

type loadResult struct {
	seen     map[string]int
	failures int
	err      error
}

func load(target string, seconds int, results chan<- loadResult) {
	script := "end=$(($(date +%%s)+%d)); while [ $(date +%%s) -lt $end ]; do wget -T 1 -qO- 'http://%s/' || echo failed; echo; done"
	output, err := command("exec", "-T", "client", "sh", "-c", fmt.Sprintf(script, seconds, target))
	if err != nil {
		results <- loadResult{err: fmt.Errorf("%w: %s", err, output)}
		return
	}

	seen, failures := tally(output)
	results <- loadResult{seen: seen, failures: failures}
}

func TestGracefulStopLosesNoRequest(t *testing.T) {
	ready(t)
	results := make(chan loadResult, 2)
	go load(anycast4, 12, results)
	go load("["+anycast6+"]", 12, results)

	time.Sleep(time.Second)
	started := time.Now()
	signal(t, "lb1", "TERM")
	eventually(t, 3*time.Second, "withdrawal on stop", func() bool { return everywhere(t, "lb2") })
	if !running("lb1") {
		t.Error("caddy stopped before the drain ended")
	}

	eventually(t, 20*time.Second, "caddy to exit", func() bool { return !running("lb1") })
	if elapsed := time.Since(started); elapsed < 3*time.Second {
		t.Errorf("caddy exited after %s, before drain_delay", elapsed)
	}

	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}

		t.Logf("%v, %d failures", result.seen, result.failures)
		if result.failures > 0 || result.seen["lb1"] == 0 {
			t.Errorf("%v, %d failures", result.seen, result.failures)
		}
	}

	for _, entry := range neighbors(t, "rt1", "lb1") {
		t.Logf("rt1 saw %q", entry.ResetReason)
		if !strings.Contains(entry.ResetReason, "Peer") && !strings.Contains(strings.ToLower(entry.ResetReason), "notification") {
			t.Errorf("rt1 saw %+v", entry)
		}
	}

	if link, err := command("exec", "-T", "lb1", "ip", "link", "show", "cbgp0"); err == nil {
		t.Errorf("the dummy interface is still there: %s", link)
	}
}

func TestKillDropsRoutes(t *testing.T) {
	ready(t)
	signal(t, "lb1", "KILL")
	elapsed := eventually(t, 15*time.Second, "routes gone after kill -9", func() bool { return everywhere(t, "lb2") })
	if elapsed > 3*time.Second {
		t.Errorf("the kernel closed the sockets, yet it took %s", elapsed)
	}

	for _, router := range routers {
		for _, path := range paths(t, router, anycast4) {
			if len(path.NextHops) > 0 && path.NextHops[0].IP == addresses["lb1"][0] {
				t.Errorf("%s kept a stale path", router)
			}
		}
	}

	startCaddy(t, "lb1")
	eventually(t, time.Minute, "return after the crash", func() bool { return everywhere(t, "lb1", "lb2") })
	shell(t, "lb1", "ip addr show dev cbgp0 | grep -q "+anycast4)
}

func onlyBy(t *testing.T, router, address string, nodes ...string) bool {
	t.Helper()

	return slices.Equal(holders(t, router, address), nodes)
}

type bfdPeer struct {
	Peer   string `json:"peer"`
	Status string `json:"status"`
}

func bfdUp(t *testing.T) bool {
	t.Helper()

	var entries []bfdPeer
	vtysh(t, "rt2", "show bfd peers json", &entries)
	return slices.Contains(entries, bfdPeer{Peer: addresses["lb1"][0], Status: "up"})
}

func TestFrozenNodeExpires(t *testing.T) {
	ready(t)
	eventually(t, 10*time.Second, "bfd up on rt2", func() bool { return bfdUp(t) })
	signal(t, "lb1", "STOP")
	defer func() { _, _ = command("exec", "-T", "lb1", "pkill", "-CONT", "-x", "caddy") }()

	started := time.Now()
	eventually(t, 5*time.Second, "bfd session on rt2 IPv4", func() bool { return onlyBy(t, "rt2", anycast4, "lb2") })
	if len(holders(t, "rt1", anycast4)) != 2 {
		t.Logf("rt1 already expired after %s", time.Since(started))
	}

	elapsed := eventually(t, 15*time.Second, "hold timer on rt1 and on IPv6", func() bool { return everywhere(t, "lb2") })
	t.Logf("expired %s after the freeze", time.Since(started).Truncate(100*time.Millisecond))
	if elapsed > 12*time.Second {
		t.Errorf("hold timer took %s", elapsed)
	}

	signal(t, "lb1", "CONT")
	eventually(t, 2*time.Minute, "return after the freeze", func() bool { return everywhere(t, "lb1", "lb2") })
}

func TestRouterRestartRecovers(t *testing.T) {
	ready(t)
	compose(t, "restart", "-t", "2", "rt2")
	eventually(t, 2*time.Minute, "rt2 to learn both nodes again", func() bool { return rt2Learned(t) })
}

func rt2Learned(t *testing.T) bool {
	t.Helper()

	if output, err := command("exec", "-T", "rt2", "vtysh", "-c", "show bgp summary json"); err != nil || !strings.HasPrefix(output, "{") {
		return false
	}

	return everywhere(t, "lb1", "lb2")
}

func TestMetrics(t *testing.T) {
	ready(t)
	metrics := shell(t, "lb1", "wget -qO- http://127.0.0.1:2019/metrics")
	for _, peer := range []string{"192.0.2.1", "192.0.2.2", "2001:db8:1::1", "2001:db8:1::2"} {
		if !strings.Contains(metrics, "peer=\""+peer+"\"") {
			t.Errorf("no metric for %s", peer)
		}
	}

	if !strings.Contains(metrics, `caddy_bgp_announced{announcement="edge",prefix="`+anycast4+`/32"} 1`) {
		t.Error("the announcement is missing")
	}
}
