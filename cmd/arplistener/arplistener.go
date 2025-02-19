package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"log"

	"github.com/joel-hk/arp"
)

var (
	ifaceFlag = flag.String("i", "eth0", "network interface to listen to")
	defaultGw = flag.String("g", "", "default gateway IPv4 (-g 192.168.1.1)")
)

func main() {
	flag.Parse()

	arp.Debug = false

	NIC := *ifaceFlag

	var err error
	HostIP, HostMAC, err := getNICInfo(NIC)
	if err != nil {
		log.Fatal("error cannot get host ip and mac ", err)
	}

	HomeLAN := net.IPNet{IP: net.IPv4(HostIP[0], HostIP[1], HostIP[2], 0), Mask: net.CIDRMask(25, 32)}
	HomeRouterIP := net.ParseIP(*defaultGw)
	if HomeRouterIP == nil {
		HomeRouterIP, err = getLinuxDefaultGateway()
	}
	if err != nil {
		log.Fatal("cannot get default gateway ", err)
	}
	log.Print("Router IP: ", HomeRouterIP, "Home LAN: ", HomeLAN)

	ctx, cancel := context.WithCancel(context.Background())

	config := arp.Config{
		NIC:     NIC,
		HostMAC: HostMAC, HostIP: HostIP,
		RouterIP: HomeRouterIP, HomeLAN: HomeLAN,
		ProbeInterval:           time.Minute * 1,
		FullNetworkScanInterval: time.Minute * 20,
		PurgeDeadline:           time.Minute * 10}
	c, err := arp.New(config)
	if err != nil {
		log.Fatal("error connection to websocket server", err)
	}
	go c.ListenAndServe(ctx)

	arpChannel := make(chan arp.MACEntry, 16)
	c.AddNotificationChannel(arpChannel)

	go arpNotification(arpChannel)

	cmd(c)

	cancel()

	c.Close()

}

func arpNotification(arpChannel chan arp.MACEntry) {
	for {
		select {
		case MACEntry := <-arpChannel:
			log.Printf("notification got ARP MACEntry for %s", MACEntry)
		}
	}
}

func cmd(c *arp.Handler) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("Command: (q)uit | (l)ist | (f)force <mac> | (s) stop <mac> | (g) toggle log")
		fmt.Print("Enter command: ")
		text, _ := reader.ReadString('\n')
		text = strings.ToLower(text[:len(text)-1])
		fmt.Println(text)

		if text == "" || len(text) < 1 {
			continue
		}

		switch text[0] {
		case 'q':
			return
		case 'g':
			if arp.Debug {
				arp.Debug = false
			} else {
				arp.Debug = true
			}
		case 'l':
			c.PrintTable()
		case 'f':
			entry, err := getMAC(c, text)
			if err != nil {
				log.Print(err)
				break
			}
			c.ForceIPChange(entry.MAC, true)
		case 's':
			MACEntry, err := getMAC(c, text)
			if err != nil {
				log.Print(err)
				break
			}
			c.StopIPChange(MACEntry.MAC)
		}
	}
}

func getMAC(c *arp.Handler, text string) (arp.MACEntry, error) {
	if len(text) <= 3 {
		return arp.MACEntry{}, fmt.Errorf("Invalid MAC")
	}
	mac, err := net.ParseMAC(text[2:])
	if err != nil {
		return arp.MACEntry{}, fmt.Errorf("Invalid MAC: %w", err)
	}

	entry, found := c.FindMAC(mac)
	if !found {
		return arp.MACEntry{}, fmt.Errorf("MAC not found")
	}
	return entry, nil
}

func getNICInfo(nic string) (ip net.IP, mac net.HardwareAddr, err error) {

	all, err := net.Interfaces()
	for _, v := range all {
		log.Print("interface name ", v.Name, v.HardwareAddr.String())
	}
	ifi, err := net.InterfaceByName(nic)
	if err != nil {
		log.Printf("NIC cannot open nic %s error %s ", nic, err)
		return ip, mac, err
	}

	mac = ifi.HardwareAddr

	addrs, err := ifi.Addrs()
	if err != nil {
		log.Printf("NIC cannot get addresses nic %s error %s ", nic, err)
		return ip, mac, err
	}

	for i := range addrs {
		tmp, _, err := net.ParseCIDR(addrs[i].String())
		if err != nil {
			log.Printf("NIC cannot parse IP %s error %s ", addrs[i].String(), err)
		}
		log.Print("IP=", tmp)
		ip = tmp.To4()
		if ip != nil && !ip.Equal(net.IPv4zero) {
			break
		}
	}

	if ip == nil || ip.Equal(net.IPv4zero) {
		err = fmt.Errorf("NIC cannot find IPv4 address list - is %s up?", nic)
		log.Print(err)
		return ip, mac, err
	}

	log.Printf("NIC successfull acquired host nic information mac=%s ip=%s", mac, ip)
	return ip, mac, err
}

const (
	file  = "/proc/net/route"
	line  = 1    // line containing the gateway addr. (first line: 0)
	sep   = "\t" // field separator
	field = 2    // field containing hex gateway address (first field: 0)
)

// NICDefaultGateway read the default gateway from linux route file
//
// file: /proc/net/route file:
//   Iface   Destination Gateway     Flags   RefCnt  Use Metric  Mask
//   eth0    00000000    C900A8C0    0003    0   0   100 00000000    0   00
//   eth0    0000A8C0    00000000    0001    0   0   100 00FFFFFF    0   00
//
func getLinuxDefaultGateway() (gw net.IP, err error) {

	file, err := os.Open(file)
	if err != nil {
		log.Print("NIC cannot open route file ", err)
		return net.IPv4zero, err
	}
	defer file.Close()

	ipd32 := net.IP{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {

		// jump to line containing the gateway address
		for i := 0; i < line; i++ {
			scanner.Scan()
		}

		// get field containing gateway address
		tokens := strings.Split(scanner.Text(), sep)
		gatewayHex := "0x" + tokens[field]

		// cast hex address to uint32
		d, _ := strconv.ParseInt(gatewayHex, 0, 64)
		d32 := uint32(d)

		// make net.IP address from uint32
		ipd32 = make(net.IP, 4)
		binary.LittleEndian.PutUint32(ipd32, d32)
		fmt.Printf("NIC default gateway is %T --> %[1]v\n", ipd32)

		// format net.IP to dotted ipV4 string
		//ip := net.IP(ipd32).String()
		//fmt.Printf("%T --> %[1]v\n", ip)

		// exit scanner
		break
	}
	return ipd32, nil
}
