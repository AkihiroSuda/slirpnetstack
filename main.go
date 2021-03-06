package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/runsc/specutils"
)

var (
	debug     bool
	debugLog  string
	netNsPath string
	ifName    string
	remoteFwd FwdAddrSlice
	localFwd  FwdAddrSlice
)

func init() {
	flag.BoolVar(&debug, "debug", false, "enable debug logging.")
	flag.StringVar(&debugLog, "debug-log", "", "additional location for logs. If it ends with '/', log files are created inside the directory with default names. The following variables are available: %TIMESTAMP%, %COMMAND%.")

	flag.StringVar(&netNsPath, "netns", "", "path to network namespace")
	flag.StringVar(&ifName, "interface", "tun0", "interface name within netns")
	flag.Var(&remoteFwd, "R", "Connections to remote side forwarded local")
	flag.Var(&localFwd, "L", "Connections to local side forwarded remote")
}

func main() {
	status := Main()
	os.Exit(status)
}

type State struct {
	RoutingDeny  []*net.IPNet
	RoutingAllow []*net.IPNet

	remoteUdpFwd map[string]*FwdAddr
	remoteTcpFwd map[string]*FwdAddr
}

func (s *State) IsUDPRPCPort(port int) bool {
	if port == 53 || port == 123 {
		return true
	}
	return false
}

func Main() int {
	var state State

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT)
	signal.Notify(sigCh, syscall.SIGTERM)

	// flag.Parse might be called from tests first. To avoid
	// duplicated items in list, ensure parsing is done only once.
	if flag.Parsed() == false {
		flag.Parse()
	}

	localFwd.SetDefaultAddrs(
		netParseIP("127.0.0.1"),
		netParseIP("10.0.2.100"))
	remoteFwd.SetDefaultAddrs(
		netParseIP("10.0.2.2"),
		netParseIP("127.0.0.1"))

	state.remoteUdpFwd = make(map[string]*FwdAddr)
	state.remoteTcpFwd = make(map[string]*FwdAddr)
	// For the list of reserved IP's see
	// https://idea.popcount.org/2019-12-06-addressing/
	state.RoutingDeny = append(state.RoutingDeny,
		MustParseCIDR("0.0.0.0/8"),
		MustParseCIDR("10.0.0.0/8"),
		MustParseCIDR("127.0.0.0/8"),
		MustParseCIDR("169.254.0.0/16"),
		MustParseCIDR("224.0.0.0/4"),
		MustParseCIDR("240.0.0.0/4"),
		MustParseCIDR("255.255.255.255/32"),
		MustParseCIDR("::/128"),
		MustParseCIDR("::1/128"),
		MustParseCIDR("::/96"),
		MustParseCIDR("::ffff:0:0:0/96"),
		MustParseCIDR("64:ff9b::/96"),
		MustParseCIDR("fc00::/7"),
		MustParseCIDR("fe80::/10"),
		MustParseCIDR("ff00::/8"),
		MustParseCIDR("fec0::/10"),
	)

	state.RoutingAllow = append(state.RoutingAllow,
		MustParseCIDR("0.0.0.0/0"),
		MustParseCIDR("::/0"),
	)

	if debug {
		log.SetLevel(log.Info)
	} else {
		log.SetLevel(log.Warning)
	}

	if debugLog != "" {
		f, err := specutils.DebugLogFile(debugLog, "slirp", "" /* name */)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening debug log file in %q: %v", debugLog, err)
			return -1
		}
		var e log.Emitter
		e = &log.GoogleEmitter{&log.Writer{Next: f}}
		log.SetTarget(e)
	}

	rand.Seed(time.Now().UnixNano())

	tunFd, tapMode, tapMtu, err := GetTunTap(netNsPath, ifName)
	if err != nil {
		return -1
	}

	s := NewStack()

	err = AddTunTap(s, 1, tunFd, tapMode, MustParseMAC("70:71:aa:4b:29:aa"), tapMtu)
	if err != nil {
		return -1
	}

	StackRoutingSetup(s, 1, "10.0.2.2/24")
	StackPrimeArp(s, 1, netParseIP("10.0.2.100"))

	StackRoutingSetup(s, 1, "2001:2::2/32")

	doneChannel := make(chan bool)

	for _, lf := range localFwd {
		var (
			err error
			srv Listener
		)
		switch lf.network {
		case "tcp":
			srv, err = LocalForwardTCP(&state, s, &lf, doneChannel)
		case "udp":
			srv, err = LocalForwardUDP(&state, s, &lf, doneChannel)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[!] Failed to listen on %s://%s:%d: %s\n",
				lf.network, lf.bind.Addr, lf.bind.Port, err)
		} else {
			laddr := srv.Addr()
			fmt.Printf("[+] local-fwd Local listen %s://%s\n",
				laddr.Network(), laddr.String())
		}
	}

	for i, rf := range remoteFwd {
		fmt.Printf("[+] Accepting on remote side %s://%s:%d\n",
			rf.network, rf.bind.Addr.String(), rf.bind.Port)
		switch rf.network {
		case "tcp":
			state.remoteTcpFwd[rf.BindAddr().String()] = &remoteFwd[i]
		case "udp":
			state.remoteUdpFwd[rf.BindAddr().String()] = &remoteFwd[i]
		}
	}

	tcpHandler := TcpRoutingHandler(&state)
	fwdTcp := tcp.NewForwarder(s, 30000, 10, tcpHandler)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwdTcp.HandlePacket)

	udpHandler := UdpRoutingHandler(&state)
	fwdUdp := udp.NewForwarder(s, udpHandler)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, fwdUdp.HandlePacket)

	// [****] Finally, the mighty event loop, waiting on signals
	pid := syscall.Getpid()
	fmt.Fprintf(os.Stderr, "[+] #%d Started\n", pid)
	syscall.Kill(syscall.Getppid(), syscall.SIGWINCH)

	for {
		select {
		case sig := <-sigCh:
			signal.Reset(sig)
			fmt.Fprintf(os.Stderr, "[-] Closing\n")
			goto stop
		}
	}
stop:
	// TODO: define semantics of graceful close on signal
	//s.Wait()
	return 0
}
