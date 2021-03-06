package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/bits"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

// Interface represents an interface
type Interface struct {
	iface   *net.Interface
	ip      net.IP
	netmask net.IPMask
	prefix  uint8
}

var (
	iface = flag.String("i", "wi-fi", "Interface to scan on")
)

func main() {
	flag.Parse()

	if scanner, err := getInterface(); err != nil {
		log.Fatal(err)
	} else if err = arpScan(scanner); err != nil {
		log.Fatal(err)
	}
}

/// Gets interface based on flag (or default wi-fi)
func getInterface() (*Interface, error) {
	var scanner Interface
	var ip net.IP
	var idx int
	ifaces, err := net.Interfaces()

	if err != nil {
		return nil, err
	}

	for _, ifs := range ifaces {
		if strings.EqualFold(ifs.Name, *iface) {
			idx = ifs.Index
			if err != nil {
				return nil, err
			}

			if !strings.Contains(ifs.Flags.String(), "up") {
				return nil, errors.New("Interface is down: " + *iface)
			}

			addrs, err := ifs.Addrs()
			if err != nil {
				return nil, err
			}

			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok {
					if ip4 := ipnet.IP.To4(); ip4 != nil {
						ip = ip4
					}
				}
			}

		}
	}

	i, err := net.InterfaceByIndex(idx)
	scanner = Interface{iface: i}

	devs, err := pcap.FindAllDevs()
	for _, dev := range devs {
		for _, addr := range dev.Addresses {
			if ip4 := addr.IP.To4(); ip4 != nil {
				if bytes.Compare(ip, ip4) == 0 {
					scanner.iface.Name = dev.Name
					scanner.ip = ip4
					scanner.netmask = addr.Netmask
					scanner.prefix = uint8(bits.OnesCount32(binary.BigEndian.Uint32(addr.Netmask)))
				}
			}
		}
	}

	return &scanner, nil
}

// arpScan scans the network using the interface provided
func arpScan(scanner *Interface) error {
	handle, err := pcap.OpenLive(scanner.iface.Name, 1024, false, pcap.BlockForever)
	if err != nil {
		return err
	}
	defer handle.Close()

	// Start reading ARP packets in a goroutine
	stop := make(chan struct{})
	go readARP(handle, scanner.iface, stop)
	defer close(stop)

	// Set up the layers
	eth := layers.Ethernet{
		SrcMAC:       scanner.iface.HardwareAddr,
		DstMAC:       net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}
	arp := layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4,
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPRequest,
		SourceHwAddress:   []byte(scanner.iface.HardwareAddr),
		SourceProtAddress: []byte(scanner.ip),
		DstHwAddress:      []byte{0, 0, 0, 0, 0, 0},
	}

	// Set up buffer and options for serialization.
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}

	log.Printf("\n[*] Scanning on %s: %s [%s/%d]\n", scanner.iface.Name, scanner.ip, scanner.ip.Mask(scanner.netmask), scanner.prefix)
	fmt.Printf("%-20s %-20s %-30s\n", "IPv4", "MAC", "Hardware")
	fmt.Println("===================================================================")

	// Start sending ARP requests
	for _, ip := range getIPAddresses(&scanner.ip, &scanner.netmask) {
		arp.DstProtAddress = []byte(ip)
		gopacket.SerializeLayers(buf, opts, &eth, &arp)
		if err := handle.WritePacketData(buf.Bytes()); err != nil {
			return err
		}
	}

	// Wait for ARP responses (tune this to network size)
	time.Sleep(time.Second * 3)

	return nil
}

func readARP(handle *pcap.Handle, iface *net.Interface, stop chan struct{}) {
	src := gopacket.NewPacketSource(handle, layers.LayerTypeEthernet)
	in := src.Packets()

	for {
		var packet gopacket.Packet
		select {
		case <-stop:
			return
		case packet = <-in:
			arpLayer := packet.Layer(layers.LayerTypeARP)
			if arpLayer == nil {
				continue
			}
			arp := arpLayer.(*layers.ARP)
			if arp.Operation != layers.ARPReply || bytes.Equal([]byte(iface.HardwareAddr), arp.SourceHwAddress) {
				// This is a packet I sent.
				continue
			}

			go examineMAC(arp.SourceProtAddress, arp.SourceHwAddress)
		}
	}
}

func examineMAC(ip, mac []byte) {
	var fabmatch string

	oui := mac[:3]
	gopath := os.Getenv("GOPATH")
	f, err := os.Open(path.Join(gopath, "src\\github.com\\jonasbostoen\\go-fingerprint\\mac-fab.txt"))

	if err != nil {
		f, err = os.Open("mac-fab.txt")
	}
	defer f.Close()

	input := bufio.NewScanner(f)
	for input.Scan() {
		line := strings.Fields(input.Text())
		macstr := line[0]
		fab := strings.Join(line[1:], " ")
		macbytes, err := hex.DecodeString(macstr)
		if err != nil {
			fmt.Println(err)
		}

		if bytes.Compare(oui, macbytes) == 0 {
			fabmatch = fab
		}
	}
	fmt.Printf("%-20v %-20v %-20s\n", net.IP(ip), net.HardwareAddr(mac), fabmatch)

}

// getIPAddresses returns all IP addresses on a subnet
func getIPAddresses(ip *net.IP, mask *net.IPMask) (out []net.IP) {
	bip := binary.BigEndian.Uint32([]byte(*ip))
	bmask := binary.BigEndian.Uint32([]byte(*mask))
	bnet := bip & bmask
	bbroadcast := bnet | ^bmask

	for bnet++; bnet < bbroadcast; bnet++ {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], bnet)
		out = append(out, net.IP(buf[:]))
	}
	return
}
