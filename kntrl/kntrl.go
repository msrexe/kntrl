package kntrl

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/sirupsen/logrus"

	"github.com/kondukto-io/kntrl/pkg/logger"
	"github.com/kondukto-io/kntrl/utils"
)

const (
	prog       = "./kntrl/bpf_bpfel_x86.o"
	rootCgroup = "/sys/fs/cgroup"
)

type ebpfProgram struct {
	Collection *ebpf.Collection
	Spec       *ebpf.CollectionSpec
}

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target=amd64  -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf ../bpf/bpf.c -- -I $BPF_HEADERS
func Run(mode uint32, hosts []net.IP) error {
	var e ebpfProgram
	defer e.clean()

	r := NewReporter()

	if !utils.IsRoot() {
		return errors.New("you need root privileges to run this program")
	}

	// allocate memory
	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	// register and load bpf program
	if err := e.load(); err != nil {
		return err
	}

	// loop and link
	for name, spec := range e.Spec.Programs {
		prg := e.Collection.Programs[name]
		logger.Log.WithFields(
			logrus.Fields{
				"name":    name,
				"program": prg,
			}).Debug("loaded program(s):")

		switch spec.Type {
		case ebpf.Kprobe:
			// link Krobe
			logger.Log.Infof("linking Kprobe [%s]", utils.ParseProgramName(prg))
			l, err := link.Kprobe(spec.AttachTo, prg, nil)
			if err != nil {
				return err
			}
			defer l.Close()

		case ebpf.Tracing:
			logger.Log.Infof("linking tracing [%s]", utils.ParseProgramName(prg))
			l, err := link.AttachTracing(link.TracingOptions{
				Program: prg,
			})
			if err != nil {
				return err
			}
			defer l.Close()

		case ebpf.TracePoint:
			logger.Log.Infof("linking tracepoint [%s]", utils.ParseProgramName(prg))
			l, err := link.Tracepoint("syscalls", "sys_enter_connect", prg, nil)
			if err != nil {
				return err
			}
			defer l.Close()

		case ebpf.CGroupSKB:
			logger.Log.Infof("linking CGroupSKB [%s]", utils.ParseProgramName(prg))
			cgroup, err := os.Open(rootCgroup)
			if err != nil {
				return err
			}
			l, err := link.AttachCgroup(link.CgroupOptions{
				Path:    cgroup.Name(),
				Attach:  ebpf.AttachCGroupInetEgress,
				Program: prg,
			})
			if err != nil {
				return err
			}
			defer l.Close()
			defer cgroup.Close()

		default:
			logger.Log.Warnf("ebpf program unrecognized: %v", prg)
		}
	}

	// set mode for filtering
	modeMap := e.Collection.Maps["mode_map"]
	if err := modeMap.Put(uint32(0), uint32(mode)); err != nil {
		logger.Log.Fatalf("failed to set mode : %s", err)
	}

	allowMap := e.Collection.Maps["allow_map"]
	key := 1
	for _, h := range hosts {
		// convert the IP bytes to __u32
		ipUint32 := binary.LittleEndian.Uint32(h)
		if err := allowMap.Put(ipUint32, uint32(key)); err != nil {
			//if err := allowMap.Put(uint32(key), ipUint32); err != nil {
			logger.Log.Fatalf("failed to update allow list (map): %s", err)
		}
	}

	ipv4EventMap := e.Collection.Maps["ipv4_events"]
	rd, err := perf.NewReader(ipv4EventMap, 4096)
	if err != nil {
		logger.Log.Fatalf("opening perf reader: %s", err)
	}
	defer rd.Close()

	ipv4ClosedMap := e.Collection.Maps["ipv4_closed_events"]
	rd2, err := perf.NewReader(ipv4ClosedMap, 4096)
	if err != nil {
		logger.Log.Fatalf("opening perf reader: %s", err)
	}
	defer rd2.Close()

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT, syscall.SIGHUP)

	// signal handler
	go func() {
		<-sigs
		done <- true

		if err := rd.Close(); err != nil {
			logger.Log.Warnf("closing perf reader: %s", err)
		}
	}()

	allowedHosts := []string{".github.com", ".kondukto.io"}

	var event IP4Event
	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				goto EXIT
			}
			logger.Log.Errorf("readig from perf event reader: %s", err)
		}

		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			logger.Log.Printf("parsing perf event: %s", err)
			continue
		}

		daddr := utils.IntToIP(event.Daddr)
		domain, err := net.LookupAddr(daddr.String())
		if err != nil {
			domain = append(domain, ".")
		}

		for i := 0; i < len(allowedHosts); i++ {
			for v := 0; v < len(domain); v++ {
				if strings.Contains(domain[v], allowedHosts[i]) {
					ipUint32 := utils.IntToIP(event.Daddr)
					if err := allowMap.Put(ipUint32, uint32(1)); err != nil {
						logger.Log.Fatalf("failed to update allow list (map): %s", err)
					}
					logger.Log.Infof("add ---->%d", ipUint32)
				}
			}
		}

		logger.Log.Infof("[%d]%-16s -> %-15s (%s) %-6d",
			event.Pid,
			event.Task,
			utils.IntToIP(event.Daddr),
			domain,
			event.Dport,
		)
	}

EXIT:
	<-done
	fmt.Println("----")
	fmt.Println()
	r.Print()
	r.Clean()
	fmt.Println("----")

	return nil
}

func (e *ebpfProgram) load() error {
	var err error
	e.Spec, err = ebpf.LoadCollectionSpec(prog)
	if err != nil {
		//logger.Log.Errorf("error loading collection spec: %v", err)
		logger.Log.Fatalf("error loading collection spec: %v", err)
		return err
	}

	e.Collection, err = ebpf.NewCollection(e.Spec)
	if err != nil {
		//logger.Log.Errorf("error new collection: %v", err)
		logger.Log.Fatalf("error new collection: %v", err)
		return err
	}

	return nil
}

// cleanup resources
// TODO: cleanup other resources (clean cgrouproot?)
// eBPF may require to delete/unregister some programs
func (e *ebpfProgram) clean() {
	e.Collection.Close()
}

// Event is a common event interface
type Event struct {
	TsUs uint64
	Pid  uint32
	Af   uint16 // Address Family
	Task [16]byte
}

// IP4Event represents a socket connect event from AF_INET(4)
type IP4Event struct {
	Event
	Daddr uint32
	Dport uint16
	// Saddr uint32
	// Sport uint16
}
