package util

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"

	goovn "github.com/ebay/go-ovn"
	utilnet "k8s.io/utils/net"
)

var NoIPError = errors.New("no IP available")

// NextIP returns IP incremented by 1
func NextIP(ip net.IP) net.IP {
	i := ipToInt(ip)
	return intToIP(i.Add(i, big.NewInt(1)))
}

func ipToInt(ip net.IP) *big.Int {
	if v := ip.To4(); v != nil {
		return big.NewInt(0).SetBytes(v)
	}
	return big.NewInt(0).SetBytes(ip.To16())
}

func intToIP(i *big.Int) net.IP {
	return net.IP(i.Bytes())
}

// ParsePortAddresses parses the MAC and IPs of the given logical switch port
func ParsePortAddresses(lsp *goovn.LogicalSwitchPort) (net.HardwareAddr, []net.IP, error) {
	var addresses []string

	if lsp.DynamicAddresses == "" {
		if len(lsp.Addresses) > 0 {
			addresses = strings.Split(lsp.Addresses[0], " ")
		}
	} else {
		// dynamic addresses have format "0a:00:00:00:00:01 192.168.1.3"
		// static addresses have format ["0a:00:00:00:00:01 192.168.1.3"]
		addresses = strings.Split(lsp.DynamicAddresses, " ")
	}

	if len(addresses) == 0 || addresses[0] == "dynamic" {
		return nil, nil, nil
	}

	mac, err := net.ParseMAC(addresses[0])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse logical switch port %q MAC %q: %v", lsp.Name, addresses[0], err)
	}
	var ips []net.IP
	for _, addr := range addresses[1:] {
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, nil, fmt.Errorf("failed to parse logical switch port %q IP %q", lsp.Name, addr)
		}
		ips = append(ips, ip)
	}
	return mac, ips, nil
}

// GetPortAddresses returns the MAC and IPs of the given logical switch port
func GetPortAddresses(portName string, ovnNBClient goovn.Client) (net.HardwareAddr, []net.IP, error) {
	lsp, err := ovnNBClient.LSPGet(portName)
	if err != nil || lsp == nil {
		// --if-exists handling in goovn
		if err == goovn.ErrorSchema || err == goovn.ErrorNotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	return ParsePortAddresses(lsp)
}

// GetLRPAddrs returns the addresses for the given logical router port
func GetLRPAddrs(portName string) ([]*net.IPNet, error) {
	networks := []*net.IPNet{}
	output, stderr, err := RunOVNNbctl("--if-exist", "get", "logical_router_port", portName, "networks")
	if err != nil {
		return nil, fmt.Errorf("failed to get logical router port %s, "+
			"stderr: %q, error: %v", portName, stderr, err)
	}

	// eg: `["100.64.0.3/16", "fd98::3/64"]`
	output = strings.Trim(output, "[]")
	if output != "" {
		for _, ipNetStr := range strings.Split(output, ", ") {
			ipNetStr = strings.Trim(ipNetStr, "\"")
			ip, cidr, err := net.ParseCIDR(ipNetStr)
			if err != nil {
				return nil, fmt.Errorf("could not parse logical router port %q: %v",
					ipNetStr, err)
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: cidr.Mask})
		}
	}
	return networks, nil
}

// GetOVSPortMACAddress returns the MAC address of a given OVS port
func GetOVSPortMACAddress(portName string) (net.HardwareAddr, error) {
	macAddress, stderr, err := RunOVSVsctl("--if-exists", "get",
		"interface", portName, "mac_in_use")
	if err != nil {
		return nil, fmt.Errorf("failed to get MAC address for %q, stderr: %q, error: %v",
			portName, stderr, err)
	}
	if macAddress == "[]" {
		return nil, fmt.Errorf("no mac_address found for %q", portName)
	}
	return net.ParseMAC(macAddress)
}

// GetNodeGatewayIfAddr returns the node logical switch gateway address
// (the ".1" address)
func GetNodeGatewayIfAddr(subnet *net.IPNet) *net.IPNet {
	return &net.IPNet{IP: NextIP(subnet.IP), Mask: subnet.Mask}
}

// GetNodeManagementIfAddr returns the node logical switch management port address
// (the ".2" address)
func GetNodeManagementIfAddr(subnet *net.IPNet) *net.IPNet {
	gwIfAddr := GetNodeGatewayIfAddr(subnet)
	return &net.IPNet{IP: NextIP(gwIfAddr.IP), Mask: subnet.Mask}
}

// GetNodeHybridOverlayIfAddr returns the node logical switch hybrid overlay
// port address (the ".3" address)
func GetNodeHybridOverlayIfAddr(subnet *net.IPNet) *net.IPNet {
	mgmtIfAddr := GetNodeManagementIfAddr(subnet)
	return &net.IPNet{IP: NextIP(mgmtIfAddr.IP), Mask: subnet.Mask}
}

// JoinHostPortInt32 is like net.JoinHostPort(), but with an int32 for the port
func JoinHostPortInt32(host string, port int32) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// SplitHostPortInt32 splits a vip into its host and port counterparts
func SplitHostPortInt32(vip string) (string, int32, error) {
	ip, portRaw, err := net.SplitHostPort(vip)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.ParseInt(portRaw, 10, 32)
	if err != nil {
		return "", 0, err
	}
	return ip, int32(port), nil
}

// IPAddrToHWAddr takes the four octets of IPv4 address (aa.bb.cc.dd, for example) and uses them in creating
// a MAC address (0A:58:AA:BB:CC:DD).  For IPv6, create a hash from the IPv6 string and use that for MAC Address.
// Assumption: the caller will ensure that an empty net.IP{} will NOT be passed.
func IPAddrToHWAddr(ip net.IP) net.HardwareAddr {
	// Ensure that for IPv4, we are always working with the IP in 4-byte form.
	ip4 := ip.To4()
	if ip4 != nil {
		// safe to use private MAC prefix: 0A:58
		return net.HardwareAddr{0x0A, 0x58, ip4[0], ip4[1], ip4[2], ip4[3]}
	}

	hash := sha256.Sum256([]byte(ip.String()))
	return net.HardwareAddr{0x0A, 0x58, hash[0], hash[1], hash[2], hash[3]}
}

// HWAddrToIPv6LLA generates the IPv6 link local address from the given hwaddr,
// with prefix 'fe80:/64'.
func HWAddrToIPv6LLA(hwaddr net.HardwareAddr) net.IP {
	return net.IP{
		0xfe,
		0x80,
		0x00,
		0x00,
		0x00,
		0x00,
		0x00,
		0x00,
		(hwaddr[0] ^ 0x02),
		hwaddr[1],
		hwaddr[2],
		0xff,
		0xfe,
		hwaddr[3],
		hwaddr[4],
		hwaddr[5],
	}
}

// JoinIPs joins the string forms of an array of net.IP, as with strings.Join
func JoinIPs(ips []net.IP, sep string) string {
	b := &strings.Builder{}
	for i, ip := range ips {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ip.String())
	}
	return b.String()
}

// JoinIPNets joins the string forms of an array of *net.IPNet, as with strings.Join
func JoinIPNets(ipnets []*net.IPNet, sep string) string {
	b := &strings.Builder{}
	for i, ipnet := range ipnets {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ipnet.String())
	}
	return b.String()
}

// JoinIPNetIPs joins the string forms of an array of *net.IPNet,
// as with strings.Join, but does not include the IP mask.
func JoinIPNetIPs(ipnets []*net.IPNet, sep string) string {
	b := &strings.Builder{}
	for i, ipnet := range ipnets {
		if i != 0 {
			b.WriteString(sep)
		}
		b.WriteString(ipnet.IP.String())
	}
	return b.String()
}

// IPFamilyName returns IP Family string based on input flag.
func IPFamilyName(isIPv6 bool) string {
	if isIPv6 {
		return "IPv6"
	} else {
		return "IPv4"
	}
}

// MatchIPFamily loops through the array of net.IP and returns a
// slice of addresses in the same IP Family, based on input flag isIPv6.
func MatchIPFamily(isIPv6 bool, ips []net.IP) ([]net.IP, error) {
	var ipAddrs []net.IP
	for _, ip := range ips {
		if utilnet.IsIPv6(ip) == isIPv6 {
			ipAddrs = append(ipAddrs, ip)
		}
	}
	if len(ipAddrs) > 0 {
		return ipAddrs, nil
	}
	return nil, fmt.Errorf("no %s IP available", IPFamilyName(isIPv6))
}

// MatchIPNetFamily loops through the array of *net.IPNet and returns the
// first entry in the list in the same IP Family, based on input flag isIPv6.
func MatchIPNetFamily(isIPv6 bool, ipnets []*net.IPNet) (*net.IPNet, error) {
	for _, ipnet := range ipnets {
		if utilnet.IsIPv6CIDR(ipnet) == isIPv6 {
			return ipnet, nil
		}
	}
	return nil, fmt.Errorf("no %s value available", IPFamilyName(isIPv6))
}

// MatchIPStringFamily loops through the array of string and returns the
// first entry in the list in the same IP Family, based on input flag isIPv6.
func MatchIPStringFamily(isIPv6 bool, ipStrings []string) (string, error) {
	for _, ipString := range ipStrings {
		if utilnet.IsIPv6String(ipString) == isIPv6 {
			return ipString, nil
		}
	}
	return "", fmt.Errorf("no %s string available", IPFamilyName(isIPv6))
}

// MatchAllIPStringFamily loops through the array of string and returns a
// of addresses in the same IP Family, based on input flag isIPv6.
func MatchAllIPStringFamily(isIPv6 bool, ipStrings []string) ([]string, error) {
	var ipAddrs []string
	for _, ipString := range ipStrings {
		if utilnet.IsIPv6String(ipString) == isIPv6 {
			ipAddrs = append(ipAddrs, ipString)
		}
	}
	if len(ipAddrs) > 0 {
		return ipAddrs, nil
	}
	return nil, NoIPError
}
