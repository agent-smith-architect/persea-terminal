package main

import (
	"flag"
	"fmt"
	"os"
	"persea-terminal/internal/ingresstest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "disposable loopback")
	socket := flag.String("socket", "", "product AF_UNIX socket")
	operator := flag.String("operator", "", "modeled login")
	probe := flag.String("probe", "", "probe base URL instead of serving")
	realm := flag.String("realm", "", "probe realm")
	session := flag.String("session", "", "probe session")
	mode := flag.String("mode", "control", "probe mode")
	sentinel := flag.String("sentinel", "PERSEA_INGRESS_PROBE", "probe control sentinel")
	mutateAlias := flag.Bool("mutate-alias", false, "prove authenticated alias mutation")
	serveTLS := flag.Bool("tls", false, "terminate TLS with an ephemeral in-memory certificate (requires ingress.hermetic_tls on the front)")
	flag.Parse()
	if *probe != "" {
		if *realm == "" || *session == "" || (*mode != "observe" && *mode != "control") {
			fmt.Fprintln(os.Stderr, "--realm, --session, and valid --mode required with --probe")
			os.Exit(2)
		}
		if err := ingresstest.Probe(*probe, *realm, *session, *mode, *sentinel, *mutateAlias); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("INGRESS_PROBE_PASS")
		return
	}
	if *socket == "" || *operator == "" {
		fmt.Fprintln(os.Stderr, "--socket and --operator required")
		os.Exit(2)
	}
	if err := ingresstest.Run(*listen, *socket, *operator, *serveTLS); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
