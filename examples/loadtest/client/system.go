package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// machine is where the run happened: the numbers mean nothing without it.
func machine() map[string]any {
	m := map[string]any{
		"os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(),
		"cores": runtime.NumCPU(), "client_gomaxprocs": runtime.GOMAXPROCS(0),
	}
	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
		m["client_nofile"] = lim.Cur
	}
	switch runtime.GOOS {
	case "darwin":
		m["cpu"] = sh("sysctl", "-n", "machdep.cpu.brand_string")
		m["os_version"] = sh("sw_vers", "-productVersion")
		if n, err := strconv.ParseInt(sh("sysctl", "-n", "hw.memsize"), 10, 64); err == nil {
			m["ram_bytes"] = n
		}
		m["ephemeral_ports"] = sh("sysctl", "-n", "net.inet.ip.portrange.first") + "-" + sh("sysctl", "-n", "net.inet.ip.portrange.last")
	case "linux":
		m["os_version"] = sh("uname", "-r")
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
					m["cpu"] = strings.TrimSpace(v)
					break
				}
			}
		}
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			var kb int64
			fmt.Sscanf(string(b), "MemTotal: %d kB", &kb)
			m["ram_bytes"] = kb * 1024
		}
		m["ephemeral_ports"] = strings.Join(strings.Fields(readFile("/proc/sys/net/ipv4/ip_local_port_range")), "-")
	}
	return m
}

func sh(name string, args ...string) string {
	b, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// raiseNofile lifts the soft limit on open files for n sockets and what the
// process needs besides.
func raiseNofile(n int) error {
	need := uint64(n) + 256
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return err
	}
	if lim.Cur < need {
		lim.Cur = need
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			return fmt.Errorf("the open-file limit is below the %d this run needs and can't be raised (%v): raise ulimit -n", need, err)
		}
	}
	return nil
}

// rssBytes is a process's resident set.
func rssBytes(pid int) int64 {
	n, _ := strconv.ParseInt(sh("ps", "-o", "rss=", "-p", strconv.Itoa(pid)), 10, 64)
	return n * 1024
}

func fetch(c *http.Client, u string) map[string]any {
	resp, err := c.Get(u)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	m := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&m)
	return m
}

func num(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// slope is the least-squares slope of y over x: what each connection adds.
func slope(x, y []float64) float64 {
	n := float64(len(x))
	if len(x) < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}
