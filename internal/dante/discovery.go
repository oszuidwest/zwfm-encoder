package dante

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

const serviceSuffix = "._netaudio-chan._udp.local."

type channelInfo struct {
	address net.IP
	port    uint16
	id      uint16
	rate    uint32
	bits    uint16
	nchan   uint16
	fppMax  uint16
	fppMin  uint16
}

func parseTXT(items []string) (channelInfo, error) {
	values := make(map[string]string)
	for _, item := range items {
		if key, value, found := strings.Cut(item, "="); found {
			values[key] = value
		}
	}
	encodingKey := "enc"
	if _, ok := values[encodingKey]; !ok {
		encodingKey = "en"
	}
	fppMaxText, fppMinText, _ := strings.Cut(values["fpp"], ",")

	id, idErr := parseTXTNumber[uint16]("id", values["id"])
	rate, rateErr := parseTXTNumber[uint32]("rate", values["rate"])
	bits, bitsErr := parseTXTNumber[uint16](encodingKey, values[encodingKey])
	nchan, nchanErr := parseTXTNumber[uint16]("nchan", values["nchan"])
	fppMax, fppMaxErr := parseTXTNumber[uint16]("fpp maximum", fppMaxText)
	fppMin, fppMinErr := parseTXTNumber[uint16]("fpp minimum", fppMinText)
	if err := errors.Join(idErr, rateErr, bitsErr, nchanErr, fppMaxErr, fppMinErr); err != nil {
		return channelInfo{}, err
	}
	if fppMax == 0 || fppMin == 0 {
		return channelInfo{}, fmt.Errorf("invalid TXT fpp %q", values["fpp"])
	}
	return channelInfo{id: id, rate: rate, bits: bits, nchan: nchan, fppMax: fppMax, fppMin: fppMin}, nil
}

// parseTXTNumber parses a decimal or 0x-prefixed hexadecimal TXT value that fits in T.
func parseTXTNumber[T uint16 | uint32](key, value string) (T, error) {
	digits, base := value, 10
	if hex, ok := strings.CutPrefix(strings.ToLower(value), "0x"); ok {
		digits, base = hex, 16
	}
	parsed, err := strconv.ParseUint(digits, base, 64)
	if err != nil || parsed > uint64(^T(0)) {
		return 0, fmt.Errorf("invalid TXT %s %q", key, value)
	}
	return T(parsed), nil
}

func buildDNSQuery(instance string) ([]byte, error) {
	name, err := dnsmessage.NewName(instance)
	if err != nil {
		return nil, fmt.Errorf("invalid discovery name %q: %w", instance, err)
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, fmt.Errorf("start DNS questions: %w", err)
	}
	questions := [...]dnsmessage.Question{
		{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET},
		{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
	}
	for index := range questions {
		if err := builder.Question(questions[index]); err != nil {
			return nil, fmt.Errorf("append DNS question: %w", err)
		}
	}
	packet, err := builder.Finish()
	if err != nil {
		return nil, fmt.Errorf("finish DNS query: %w", err)
	}
	return packet, nil
}

func parseDNSResponse(packet []byte, requested string) (uint16, *channelInfo, error) {
	var parser dnsmessage.Parser
	if _, err := parser.Start(packet); err != nil {
		return 0, nil, fmt.Errorf("parse DNS header: %w", err)
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return 0, nil, fmt.Errorf("parse DNS questions: %w", err)
	}

	resources, err := parser.AllAnswers()
	if err != nil {
		return 0, nil, fmt.Errorf("parse DNS answers: %w", err)
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		return 0, nil, fmt.Errorf("parse DNS authorities: %w", err)
	}
	additionals, err := parser.AllAdditionals()
	if err != nil {
		return 0, nil, fmt.Errorf("parse DNS additionals: %w", err)
	}

	var port uint16
	var txt *channelInfo
	var txtErr error
	resources = append(resources, additionals...)
	for index := range resources {
		resource := &resources[index]
		if !strings.EqualFold(resource.Header.Name.String(), requested) {
			continue
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.SRVResource:
			port = body.Port
		case *dnsmessage.TXTResource:
			parsed, err := parseTXT(body.TXT)
			if err != nil {
				txtErr = fmt.Errorf("invalid TXT record for %q: %w", requested, err)
				continue
			}
			txt = &parsed
		}
	}
	if txt == nil && txtErr != nil {
		return port, nil, txtErr
	}
	return port, txt, nil
}

func discoveryInterfaces(interfaceID string) ([]net.Interface, error) {
	if interfaceID != "" {
		iface, err := net.InterfaceByName(interfaceID)
		if err == nil {
			err = checkDiscoveryInterface(iface)
		}
		if err != nil {
			return nil, fmt.Errorf("network interface %q: %w", interfaceID, err)
		}
		return []net.Interface{*iface}, nil
	}

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	usable := slices.DeleteFunc(interfaces, func(iface net.Interface) bool {
		return checkDiscoveryInterface(&iface) != nil
	})
	if len(usable) == 0 {
		return nil, errors.New("no up, multicast-capable network interface with a non-loopback IPv4 address")
	}
	return usable, nil
}

// checkDiscoveryInterface returns why iface cannot carry discovery, or nil.
func checkDiscoveryInterface(iface *net.Interface) error {
	const required = net.FlagUp | net.FlagMulticast
	if iface.Flags&required != required || iface.Flags&net.FlagLoopback != 0 {
		return errors.New("must be up, multicast-capable, and non-loopback")
	}
	_, err := firstIPv4Address(iface)
	return err
}

func firstIPv4Address(iface *net.Interface) (net.IP, error) {
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok {
			continue
		}
		ip := network.IP.To4()
		if ip != nil && !ip.IsLoopback() {
			return ip, nil
		}
	}
	return nil, errors.New("no non-loopback IPv4 address")
}

func discoverChannel(ctx context.Context, interfaces []net.Interface, transmitter, channel string, requiredSource net.IP) (channelInfo, error) {
	instance := channel + "@" + transmitter + serviceSuffix
	query, err := buildDNSQuery(instance)
	if err != nil {
		return channelInfo{}, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return channelInfo{}, fmt.Errorf("open discovery socket: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	packetConn := ipv4.NewPacketConn(conn)
	lookupCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	stopInterrupt := context.AfterFunc(lookupCtx, func() {
		_ = conn.SetReadDeadline(time.Now())
	})
	defer stopInterrupt()

	destination := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	buffer := make([]byte, 65535)
	nextQuery := time.Time{}
	var info channelInfo
	var lastParseErr error

	for {
		now := time.Now()
		if !now.Before(nextQuery) {
			if err := sendDiscoveryQuery(packetConn, interfaces, query, destination); err != nil {
				return channelInfo{}, fmt.Errorf("send discovery query for %q: %w", channel, err)
			}
			nextQuery = now.Add(time.Second)
		}
		deadline := nextQuery
		if contextDeadline, ok := lookupCtx.Deadline(); ok {
			deadline = earlierTime(deadline, contextDeadline)
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return channelInfo{}, fmt.Errorf("set discovery deadline: %w", err)
		}
		n, source, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if lookupErr := lookupCtx.Err(); lookupErr != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return channelInfo{}, fmt.Errorf("discover channel %q: %w", channel, contextErr)
				}
				if lastParseErr != nil {
					return channelInfo{}, fmt.Errorf(
						"discover channel %q timed out after malformed response: %w",
						channel,
						lastParseErr,
					)
				}
				return channelInfo{}, fmt.Errorf("discover channel %q: %w", channel, lookupErr)
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return channelInfo{}, fmt.Errorf("receive discovery response for %q: %w", channel, err)
		}

		complete, parseErr := mergeDiscoveryResponse(&info, buffer[:n], source.IP, instance, requiredSource)
		if parseErr != nil {
			lastParseErr = parseErr
			continue
		}
		if complete {
			return info, nil
		}
	}
}

// sendDiscoveryQuery sends query on every interface and fails only when no
// interface could send it.
func sendDiscoveryQuery(conn *ipv4.PacketConn, interfaces []net.Interface, query []byte, destination *net.UDPAddr) error {
	var errs []error
	for index := range interfaces {
		err := conn.SetMulticastInterface(&interfaces[index])
		if err == nil {
			_, err = conn.WriteTo(query, nil, destination)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == len(interfaces) {
		return errors.Join(errs...)
	}
	return nil
}

func mergeDiscoveryResponse(info *channelInfo, packet []byte, source net.IP, requested string, requiredSource net.IP) (bool, error) {
	source = source.To4()
	if source == nil {
		return false, nil
	}
	if requiredSource != nil && !source.Equal(requiredSource) {
		return false, nil
	}
	if info.address != nil && !source.Equal(info.address) {
		return false, nil
	}

	port, txt, err := parseDNSResponse(packet, requested)
	if err != nil {
		return false, err
	}
	if port == 0 && txt == nil {
		return false, nil
	}
	if info.address == nil {
		info.address = source
	}
	if port != 0 {
		info.port = port
	}
	if txt != nil {
		txt.address, txt.port = info.address, info.port
		*info = *txt
	}
	// A port comes from SRV; parseTXT guarantees a non-zero fppMax.
	return info.port != 0 && info.fppMax != 0, nil
}
