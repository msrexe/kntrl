package tracer

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
	"github.com/kondukto-io/kntrl/internal/core/domain"
	ebpfman "github.com/kondukto-io/kntrl/pkg/ebpf"
	"github.com/kondukto-io/kntrl/pkg/logger"
	"github.com/kondukto-io/kntrl/pkg/reporter.go"
	"github.com/kondukto-io/kntrl/utils"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	prog       = "./kntrl/bpf_bpfel_x86.o"
	rootCgroup = "/sys/fs/cgroup"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target=arm64  -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf ../../../bpf/bpf.c -- -I $BPF_HEADERS
func Run(cmd cobra.Command) error {
	var tracerMode = cmd.Flag("mode").Value.String()
	if tracerMode == "" {
		return errors.New("[mode] flag is required")
	}

	if tracerMode != domain.TracerModeMonitor && tracerMode != domain.TracerModeTrace {
		return fmt.Errorf("[mode] flag is invalid: %s", tracerMode)
	}

	var allowedHosts = cmd.Flag("hosts").Value.String()
	if allowedHosts == "" {
		logger.Log.Debugf("no provided allowed hosts")
	}

	allowedIPS, err := utils.ParseHosts(allowedHosts)
	if err != nil {
		return fmt.Errorf("failed to parse allowed hosts: %s", err)
	}

	if !utils.IsRoot() {
		return errors.New("you need root privileges to run this program")
	}

	ebpfClient := ebpfman.New()
	if err := ebpfClient.Load(prog); err != nil {
		logger.Log.Fatalf("failed to load ebpf program: %s", err)
	}

	defer ebpfClient.Clean()

	switch tracerMode {
	case domain.TracerModeTrace:
		// set mode for filtering
		modeMap := ebpfClient.Collection.Maps[domain.EBPFCollectionMapMode]
		if err := modeMap.Put(uint32(0), uint32(domain.TracerModeIndexTrace)); err != nil {
			logger.Log.Fatalf("failed to set mode: %v", err)
		}

	case domain.TracerModeMonitor:
		// set mode for filtering
		modeMap := ebpfClient.Collection.Maps[domain.EBPFCollectionMapMode]
		if err := modeMap.Put(uint32(0), uint32(domain.TracerModeIndexMonitor)); err != nil {
			logger.Log.Fatalf("failed to set mode: %v", err)
		}

	default:
		return fmt.Errorf("invalid mode: %s", tracerMode)
	}

	allowMap := ebpfClient.Collection.Maps[domain.EBPFCollectionMapAllow]

	for _, ip := range allowedIPS {
		// convert the IP bytes to __u32
		ipUint32 := binary.LittleEndian.Uint32(ip)
		if err := allowMap.Put(ipUint32, uint32(1)); err != nil {
			// if err := allowMap.Put(uint32(key), ipUint32); err != nil {
			logger.Log.Fatalf("failed to update allow list (map): %s", err)
		}
	}

	ipv4EventMap := ebpfClient.Collection.Maps[domain.EBPFCollectionMapIPV4Events]
	ipV4Events, err := perf.NewReader(ipv4EventMap, 4096)
	if err != nil {
		logger.Log.Fatalf("failed to read ipv4 events: %s", err)
	}

	defer ipV4Events.Close()

	ipv4ClosedMap := ebpfClient.Collection.Maps[domain.EBPFCollectionMapIPV4ClosedEvents]
	ipV4ClosedEvent, err := perf.NewReader(ipv4ClosedMap, 4096)
	if err != nil {
		logger.Log.Fatalf("failed to read ipv4 closed events: %s", err)
	}

	defer ipV4ClosedEvent.Close()

	r := reporter.NewReporter()
	if r.Err != nil {
		logger.Log.Fatalf("failed to read ipv4 closed events: %s", err)
	}

	// allocate memory
	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	// loop and link
	for name, spec := range ebpfClient.Spec.Programs {
		prg := ebpfClient.Collection.Programs[name]
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

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT, syscall.SIGHUP)

	// signal handler
	go func() {
		<-sigs
		done <- true

		if err := ipV4Events.Close(); err != nil {
			logger.Log.Warnf("closing perf reader: %s", err)
		}
	}()

	allowedHostsAddress := []string{".github.com", ".kondukto.io"}

	var event domain.IP4Event
	for {
		record, err := ipV4Events.Read()
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
				if strings.Contains(domain[v], allowedHostsAddress[i]) {
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
