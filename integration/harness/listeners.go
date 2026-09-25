package harness

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// ErrListenersUnsupported is returned where /proc cannot be read.
var ErrListenersUnsupported = errors.New("listening ports can be read only on Linux")

// tcpListen is the socket state /proc/net/tcp reports for LISTEN.
const tcpListen = "0A"

// ListeningPorts returns the TCP ports process pid is listening on, sorted.
// It matches the process's socket inodes against /proc/<pid>/net/tcp{,6}.
func ListeningPorts(pid int) ([]int, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrListenersUnsupported
	}
	inodes, err := socketInodes(pid)
	if err != nil {
		return nil, err
	}
	var ports []int
	for _, table := range []string{"tcp", "tcp6"} {
		found, err := listeningInTable(filepath.Join("/proc", strconv.Itoa(pid), "net", table), inodes)
		if err != nil {
			return nil, err
		}
		ports = append(ports, found...)
	}
	slices.Sort(ports)
	return slices.Compact(ports), nil
}

func socketInodes(pid int) (map[string]bool, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	inodes := map[string]bool{}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue // the descriptor closed while we looked
		}
		if inode, ok := strings.CutPrefix(target, "socket:["); ok {
			inodes[strings.TrimSuffix(inode, "]")] = true
		}
	}
	return inodes, nil
}

func listeningInTable(path string, inodes map[string]bool) ([]int, error) {
	f, err := os.Open(path) // #nosec G304 -- a /proc path the harness builds
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	var ports []int
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != tcpListen || !inodes[fields[9]] {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("parsing port %q: %w", hexPort, err)
		}
		ports = append(ports, int(port))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return ports, nil
}

// Ports returns the numeric ports of the collector's three addresses.
func (c *Collector) Ports() (listen, health, metrics int, err error) {
	if listen, err = portOf(c.ListenAddr); err != nil {
		return 0, 0, 0, err
	}
	if health, err = portOf(c.HealthAddr); err != nil {
		return 0, 0, 0, err
	}
	if metrics, err = portOf(c.MetricsAddr); err != nil {
		return 0, 0, 0, err
	}
	return listen, health, metrics, nil
}
