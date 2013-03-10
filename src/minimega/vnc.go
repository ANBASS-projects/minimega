package main

import (
	"fmt"
	log "minilog"
	"novnctun"
	"os"
	"strconv"
	"strings"
)

var (
	vnc_server *novnctun.Tun
	vnc_novnc  string = "misc/novnc"
)

const vnc_port = ":8080"

// register a Hosts() function on type vm_list, allowing us to point novnctun at it
func (vms *vm_list) Hosts() map[string][]string {
	ret := make(map[string][]string)

	// the vnc port is just 5900 + the vm id

	// first grab our own list of hosts
	host, err := os.Hostname()
	if err != nil {
		log.Errorln(err)
		return nil
	}
	for _, vm := range vms.vms {
		if vm.State != VM_QUIT && vm.State != VM_ERROR {
			port := fmt.Sprintf("%v", 5900+vm.Id)
			ret[host] = append(ret[host], port)
		}
	}

	// get a list of the other hosts on the network
	cmd := cli_command{
		Args: []string{"hostname"},
	}
	resp := meshageBroadcast(cmd)
	if resp.Error != "" {
		log.Errorln(resp.Error)
		return nil
	}

	hosts := strings.Fields(resp.Response)

	for _, h := range hosts {
		// get a list of vms from that host
		cmd := cli_command{
			Args: []string{h, "vm_status"},
		}
		resp := meshageSet(cmd)
		if resp.Error != "" {
			log.Errorln(resp.Error)
			continue // don't error out if just one host fails us
		}

		lines := strings.Split(resp.Response, "\n")
		for _, l := range lines {
			// the vm id is the second field
			// TODO: filter out any quit or error state vms from remote vnc lsit
			f := strings.Fields(l)
			if len(f) > 2 {
				val, err := strconv.Atoi(f[1])
				if err != nil {
					log.Errorln(err)
					continue
				}
				port := fmt.Sprintf("%v", 5900+val)
				ret[h] = append(ret[h], port)
			}
		}
	}
	return ret
}

func cli_vnc(c cli_command) cli_response {
	// we have 2 possible cases:
	// vnc novnc - set the vnc path
	// vnc serve :8080 serve on a specific port and don't launch anything
	if len(c.Args) == 0 {
		return cli_response{
			Error: "vnc takes at least one argument",
		}
	}
	switch c.Args[0] {
	case "novnc":
		if len(c.Args) == 1 {
			return cli_response{
				Response: vnc_novnc,
			}
		} else if len(c.Args) > 2 {
			return cli_response{
				Error: "vnc novnc takes 2 arguments",
			}
		}
		vnc_novnc = c.Args[1]
	case "serve":
		if len(c.Args) == 1 { // just start the server
			if vnc_server == nil {
				vnc_serve(vnc_port)
			} else {
				e := fmt.Sprintf("vnc already running on: %v", vnc_server.Addr)
				return cli_response{
					Error: e,
				}
			}
		} else if len(c.Args) == 2 {
			if vnc_server == nil {
				vnc_serve(c.Args[1])
			} else {
				e := fmt.Sprintf("vnc already running on: %v", vnc_server.Addr)
				return cli_response{
					Error: e,
				}
			}
		} else {
			return cli_response{
				Error: "invalid command",
			}
		}
	default: // must be an id right?
		return cli_response{
			Error: "invalid command",
		}
	}
	return cli_response{}
}

func vnc_serve(addr string) {
	vnc_server = &novnctun.Tun{
		Addr:   addr,
		Hosts:  &vms,
		Files:  vnc_novnc,
		Unsafe: false,
	}
	go func() {
		log.Errorln(vnc_server.Start())
	}()
}
