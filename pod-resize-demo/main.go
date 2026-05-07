package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// allocated holds memory blocks so GC doesn't reclaim them.
var (
	allocated [][]byte
	allocMu   sync.Mutex
)

// ResourceLimits reads the container's CPU and memory limits from cgroup v2.
type ResourceLimits struct {
	CPULimit    string `json:"cpu_limit"`
	MemoryLimit string `json:"memory_limit"`
	CPURequest  string `json:"cpu_request_env"`
	MemLimit    string `json:"mem_limit_env"`
}

// MemStats holds Go runtime memory statistics.
type MemStats struct {
	Alloc      uint64 `json:"alloc_bytes"`
	TotalAlloc uint64 `json:"total_alloc_bytes"`
	Sys        uint64 `json:"sys_bytes"`
	NumGC      uint32 `json:"num_gc"`
}

// AppInfo is the top-level response for the root handler.
type AppInfo struct {
	Hostname      string         `json:"hostname"`
	Timestamp     string         `json:"timestamp"`
	GoVersion     string         `json:"go_version"`
	Limits        ResourceLimits `json:"cgroup_limits"`
	Memory        MemStats       `json:"runtime_memory"`
	AllocatedMiB  int            `json:"manually_allocated_mib"`
	ResizeFeature string         `json:"resize_feature"`
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(data))
}

// cgroupV2CPULimit converts the cgroup cpu.max value to a human-readable string.
func cgroupV2CPULimit() string {
	raw := readFile("/sys/fs/cgroup/cpu.max")
	if raw == "unavailable" {
		return raw
	}
	parts := strings.Fields(raw)
	if len(parts) != 2 || parts[0] == "max" {
		return "unlimited"
	}
	quota, err1 := strconv.ParseFloat(parts[0], 64)
	period, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		return raw
	}
	cores := quota / period
	return fmt.Sprintf("%.3f cores", cores)
}

// cgroupV2MemLimit returns the memory.max value in MiB.
func cgroupV2MemLimit() string {
	raw := readFile("/sys/fs/cgroup/memory.max")
	if raw == "unavailable" || raw == "max" {
		return "unlimited"
	}
	bytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return raw
	}
	return fmt.Sprintf("%d MiB", bytes/(1024*1024))
}

// cgroupV1MemLimit falls back to cgroup v1.
func cgroupV1MemLimit() string {
	raw := readFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if raw == "unavailable" {
		return raw
	}
	bytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return raw
	}
	if bytes > 1<<40 {
		return "unlimited"
	}
	return fmt.Sprintf("%d MiB", bytes/(1024*1024))
}

// cgroupV1CPU reads cpu.cfs_quota_us and cpu.cfs_period_us for cgroup v1.
func cgroupV1CPU() string {
	quota := readFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period := readFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if quota == "unavailable" || quota == "-1" {
		return "unlimited"
	}
	q, err1 := strconv.ParseFloat(quota, 64)
	p, err2 := strconv.ParseFloat(period, 64)
	if err1 != nil || err2 != nil || p == 0 {
		return "unavailable"
	}
	return fmt.Sprintf("%.3f cores", q/p)
}

func detectCgroupVersion() int {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return 2
		}
	}
	return 1
}

func getLimits() ResourceLimits {
	cgVer := detectCgroupVersion()
	var cpuLimit, memLimit string
	if cgVer == 2 {
		cpuLimit = cgroupV2CPULimit()
		memLimit = cgroupV2MemLimit()
	} else {
		cpuLimit = cgroupV1CPU()
		memLimit = cgroupV1MemLimit()
	}
	return ResourceLimits{
		CPULimit:    cpuLimit,
		MemoryLimit: memLimit,
		CPURequest:  os.Getenv("MY_CPU_REQUEST"),
		MemLimit:    os.Getenv("MY_MEM_LIMIT"),
	}
}

func getMemStats() MemStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return MemStats{
		Alloc:      ms.Alloc,
		TotalAlloc: ms.TotalAlloc,
		Sys:        ms.Sys,
		NumGC:      ms.NumGC,
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func allocatedMiB() int {
	allocMu.Lock()
	defer allocMu.Unlock()
	total := 0
	for _, b := range allocated {
		total += len(b)
	}
	return total / (1024 * 1024)
}

// rootHandler returns a JSON snapshot of the pod's resource state.
func rootHandler(w http.ResponseWriter, r *http.Request) {
	info := AppInfo{
		Hostname:      hostname(),
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		GoVersion:     runtime.Version(),
		Limits:        getLimits(),
		Memory:        getMemStats(),
		AllocatedMiB:  allocatedMiB(),
		ResizeFeature: "InPlacePodVerticalScaling",
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(info)
}

// allocHandler allocates the requested number of MiB and holds the reference.
// Usage: POST /alloc?mib=64
func allocHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mibStr := r.URL.Query().Get("mib")
	mib, err := strconv.Atoi(mibStr)
	if err != nil || mib <= 0 || mib > 512 {
		http.Error(w, "provide mib=1..512", http.StatusBadRequest)
		return
	}
	block := make([]byte, mib*1024*1024)
	// Touch every page so the OS actually allocates physical memory.
	for i := range block {
		block[i] = byte(i & 0xff)
	}
	allocMu.Lock()
	allocated = append(allocated, block)
	allocMu.Unlock()
	log.Printf("allocated %d MiB (total: %d MiB)", mib, allocatedMiB())
	fmt.Fprintf(w, "allocated %d MiB; total held: %d MiB\n", mib, allocatedMiB())
}

// freeHandler releases all manually allocated memory.
func freeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	allocMu.Lock()
	allocated = nil
	allocMu.Unlock()
	runtime.GC()
	log.Println("released all allocated memory")
	fmt.Fprintln(w, "released all manually allocated memory")
}

// healthzHandler is a simple liveness probe endpoint.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprintln(w, "ok")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/", rootHandler)
	http.HandleFunc("/healthz", healthzHandler)
	http.HandleFunc("/alloc", allocHandler)
	http.HandleFunc("/free", freeHandler)

	log.Printf("pod-resize-demo starting on :%s", port)
	log.Printf("Endpoints: / (status)  /alloc?mib=N (POST)  /free (POST)  /healthz")
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
