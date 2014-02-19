package net

import (
	bosherr "bosh/errors"
	boshsettings "bosh/settings"
	boshsys "bosh/system"
	"bytes"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

type centos struct {
	arpWaitInterval time.Duration
	cmdRunner       boshsys.CmdRunner
	fs              boshsys.FileSystem
}

func NewCentosNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	arpWaitInterval time.Duration,
) (net centos) {
	net.arpWaitInterval = arpWaitInterval
	net.cmdRunner = cmdRunner
	net.fs = fs
	return
}

func (net centos) getDnsServers(networks boshsettings.Networks) (dnsServers []string) {
	dnsNetwork, found := networks.DefaultNetworkFor("dns")
	if found {
		for i := len(dnsNetwork.Dns) - 1; i >= 0; i-- {
			dnsServers = append(dnsServers, dnsNetwork.Dns[i])
		}
	}

	return
}

func (net centos) SetupDhcp(networks boshsettings.Networks) (err error) {
	dnsServers := []string{}
	dnsNetwork, found := networks.DefaultNetworkFor("dns")
	if found {
		for i := len(dnsNetwork.Dns) - 1; i >= 0; i-- {
			dnsServers = append(dnsServers, dnsNetwork.Dns[i])
		}
	}

	type dhcpConfigArg struct {
		DnsServers []string
	}

	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(CENTOS_DHCP_CONFIG_TEMPLATE))

	err = t.Execute(buffer, dhcpConfigArg{dnsServers})
	if err != nil {
		err = bosherr.WrapError(err, "Generating config from template")
		return
	}

	written, err := net.fs.ConvergeFileContents("/etc/dhcp/dhclient.conf", buffer.Bytes())
	if err != nil {
		err = bosherr.WrapError(err, "Writing to /etc/dhcp/dhclient.conf")
		return
	}

	if written {
		// Ignore errors here, just run the commands
		net.cmdRunner.RunCommand("service", "network", "restart")
	}

	return
}

// DHCP Config file - /etc/dhcp3/dhclient.conf
const CENTOS_DHCP_CONFIG_TEMPLATE = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name "<hostname>";

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;

{{ range .DnsServers }}prepend domain-name-servers {{ . }};
{{ end }}`

func (net centos) SetupManualNetworking(networks boshsettings.Networks) (err error) {
	modifiedNetworks, err := net.writeIfcfgs(networks)
	if err != nil {
		err = bosherr.WrapError(err, "Writing network interfaces")
		return
	}

	net.restartNetwork()

	err = net.writeResolvConf(networks)
	if err != nil {
		err = bosherr.WrapError(err, "Writing resolv.conf")
		return
	}

	go net.gratuitiousArp(modifiedNetworks)

	return
}

func (net centos) gratuitiousArp(networks []CustomNetwork) {
	for i := 0; i < 6; i++ {
		for _, network := range networks {
			for !net.fs.FileExists(filepath.Join("/sys/class/net", network.Interface)) {
				time.Sleep(100 * time.Millisecond)
			}

			net.cmdRunner.RunCommand("arping", "-c", "1", "-U", "-I", network.Interface, network.Ip)
			time.Sleep(net.arpWaitInterval)
		}
	}
	return
}

func (net centos) writeIfcfgs(networks boshsettings.Networks) (modifiedNetworks []CustomNetwork, err error) {
	macAddresses, err := net.detectMacAddresses()
	if err != nil {
		err = bosherr.WrapError(err, "Detecting mac addresses")
		return
	}

	for _, aNet := range networks {
		var network, broadcast string
		network, broadcast, err = boshsys.CalculateNetworkAndBroadcast(aNet.Ip, aNet.Netmask)
		if err != nil {
			err = bosherr.WrapError(err, "Calculating network and broadcast")
			return
		}

		newNet := CustomNetwork{
			aNet,
			macAddresses[aNet.Mac],
			network,
			broadcast,
			true,
		}
		modifiedNetworks = append(modifiedNetworks, newNet)

		buffer := bytes.NewBuffer([]byte{})
		t := template.Must(template.New("ifcfg").Parse(CENTOS_IFCFG_TEMPLATE))

		err = t.Execute(buffer, newNet)
		if err != nil {
			err = bosherr.WrapError(err, "Generating config from template")
			return
		}

		err = net.fs.WriteFile(filepath.Join("/etc/sysconfig/network-scripts", "ifcfg-"+newNet.Interface), buffer.Bytes())
		if err != nil {
			err = bosherr.WrapError(err, "Writing to /etc/sysconfig/network-scripts")
			return
		}
	}

	return
}

const CENTOS_IFCFG_TEMPLATE = `DEVICE={{ .Interface }}
BOOTPROTO=static
IPADDR={{ .Ip }}
NETMASK={{ .Netmask }}
BROADCAST={{ .Broadcast }}
{{ if .HasDefaultGateway }}GATEWAY={{ .Gateway }}{{ end }}
ONBOOT=yes`

func (p centos) writeResolvConf(networks boshsettings.Networks) (err error) {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("resolv-conf").Parse(CENTOS_RESOLV_CONF_TEMPLATE))

	dnsServers := p.getDnsServers(networks)
	dnsServersArg := dnsConfigArg{dnsServers}
	err = t.Execute(buffer, dnsServersArg)
	if err != nil {
		err = bosherr.WrapError(err, "Generating config from template")
		return
	}

	err = p.fs.WriteFile("/etc/resolv.conf", buffer.Bytes())
	if err != nil {
		err = bosherr.WrapError(err, "Writing to /etc/resolv.conf")
		return
	}

	return
}

const CENTOS_RESOLV_CONF_TEMPLATE = `{{ range .DnsServers }}nameserver {{ . }}
{{ end }}`

func (net centos) detectMacAddresses() (addresses map[string]string, err error) {
	addresses = map[string]string{}

	filePaths, err := net.fs.Glob("/sys/class/net/*")
	if err != nil {
		err = bosherr.WrapError(err, "Getting file list from /sys/class/net")
		return
	}

	var macAddress string
	for _, filePath := range filePaths {
		macAddress, err = net.fs.ReadFileString(filepath.Join(filePath, "address"))
		if err != nil {
			err = bosherr.WrapError(err, "Reading mac address from file")
			return
		}

		macAddress = strings.Trim(macAddress, "\n")

		interfaceName := filepath.Base(filePath)
		addresses[macAddress] = interfaceName
	}

	return
}

func (net centos) restartNetwork() {
	net.cmdRunner.RunCommand("service", "network", "restart")
	return
}
