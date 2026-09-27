package dante

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	hasSRV  bool
	hasTXT  bool
}

func parseTXT(items []string) (channelInfo, error) {
	values := make(map[string]string)
	for _, item := range items {
		key, value, found := strings.Cut(item, "=")
		if found {
			values[key] = value
		}
	}

	id, err := parseUint16(values, "id", true)
	if err != nil {
		return channelInfo{}, err
	}
	rate, err := parseUint32(values, "rate")
	if err != nil {
		return channelInfo{}, err
	}
	encodingKey := "enc"
	if _, ok := values[encodingKey]; !ok {
		encodingKey = "en"
	}
	bits, err := parseUint16(values, encodingKey, false)
	if err != nil {
		return channelInfo{}, fmt.Errorf("bits per sample: %w", err)
	}
	nchan, err := parseUint16(values, "nchan", false)
	if err != nil {
		return channelInfo{}, err
	}

	fppValue, ok := values["fpp"]
	if !ok {
		return channelInfo{}, errors.New("missing TXT key \"fpp\"")
	}
	fppParts := strings.Split(fppValue, ",")
	if len(fppParts) != 2 {
		return channelInfo{}, fmt.Errorf("invalid TXT fpp %q", fppValue)
	}
	fppMaxValue, err := strconv.ParseUint(fppParts[0], 10, 16)
	if err != nil || fppMaxValue == 0 {
		return channelInfo{}, fmt.Errorf("invalid TXT fpp maximum %q", fppParts[0])
	}
	fppMinValue, err := strconv.ParseUint(fppParts[1], 10, 16)
	if err != nil || fppMinValue == 0 {
		return channelInfo{}, fmt.Errorf("invalid TXT fpp minimum %q", fppParts[1])
	}

	return channelInfo{
		id:     id,
		rate:   rate,
		bits:   bits,
		nchan:  nchan,
		fppMax: uint16(fppMaxValue),
		fppMin: uint16(fppMinValue),
		hasTXT: true,
	}, nil
}

func parseUint16(values map[string]string, key string, allowHex bool) (uint16, error) {
	value, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("missing TXT key %q", key)
	}
	base := 10
	if allowHex && (strings.HasPrefix(value, "0x") || strings.HasPrefix(value, "0X")) {
		base = 16
		value = value[2:]
	}
	parsed, err := strconv.ParseUint(value, base, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid TXT %s %q", key, value)
	}
	return uint16(parsed), nil
}

func parseUint32(values map[string]string, key string) (uint32, error) {
	value, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("missing TXT key %q", key)
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid TXT %s %q", key, value)
	}
	return uint32(parsed), nil
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

	var port uint16
	var txt *channelInfo
	var txtErr error
	handle := func(resource dnsmessage.Resource) {
		if !strings.EqualFold(resource.Header.Name.String(), requested) {
			return
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.SRVResource:
			port = body.Port
		case *dnsmessage.TXTResource:
			parsed, err := parseTXT(body.TXT)
			if err != nil {
				txtErr = fmt.Errorf("invalid TXT record for %q: %w", requested, err)
				return
			}
			txt = &parsed
		}
	}

	for {
		resource, err := parser.Answer()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("parse DNS answer: %w", err)
		}
		handle(resource)
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		return 0, nil, fmt.Errorf("parse DNS authorities: %w", err)
	}
	for {
		resource, err := parser.Additional()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return 0, nil, fmt.Errorf("parse DNS additional: %w", err)
		}
		handle(resource)
	}
	if txt == nil && txtErr != nil {
		return port, nil, txtErr
	}
	return port, txt, nil
}

func discoveryInterfaces(interfaceID string) ([]net.Interface, error) {
	interfaces := make([]net.Interface, 0)
	if interfaceID != "" {
		iface, err := net.InterfaceByName(interfaceID)
		if err != nil {
			return nil, fmt.Errorf("network interface %q: %w", interfaceID, err)
		}
		interfaces = append(interfaces, *iface)
	} else {
		available, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("list network interfaces: %w", err)
		}
		interfaces = available
	}

	usable := make([]net.Interface, 0, len(interfaces))
	for index := range interfaces {
		iface := interfaces[index]
		validFlags := iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0
		if !validFlags || iface.Flags&net.FlagLoopback != 0 {
			if interfaceID != "" {
				return nil, fmt.Errorf(
					"network interface %q must be up, multicast-capable, and non-loopback",
					interfaceID,
				)
			}
			continue
		}
		if _, err := firstIPv4Address(&iface); err != nil {
			if interfaceID != "" {
				return nil, fmt.Errorf("network interface %q: %w", interfaceID, err)
			}
			continue
		}
		usable = append(usable, iface)
	}
	if len(usable) == 0 {
		return nil, errors.New("no up, multicast-capable network interface with a non-loopback IPv4 address")
	}
	return usable, nil
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
			return append(net.IP(nil), ip...), nil
		}
	}
	return nil, errors.New("no non-loopback IPv4 address")
}

func discoverChannel(
	ctx context.Context,
	interfaces []net.Interface,
	transmitter string,
	channel string,
	requiredSource net.IP,
) (channelInfo, error) {
	instance := channel + "@" + transmitter + serviceSuffix
	query, err := buildDNSQuery(instance)
	if err != nil {
		return channelInfo{}, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
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
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				continue
			}
			return channelInfo{}, fmt.Errorf("receive discovery response for %q: %w", channel, err)
		}

		complete, parseErr := mergeDiscoveryResponse(
			&info,
			buffer[:n],
			source.IP,
			instance,
			requiredSource,
		)
		if parseErr != nil {
			lastParseErr = parseErr
			continue
		}
		if complete {
			return info, nil
		}
	}
}

func sendDiscoveryQuery(
	conn *ipv4.PacketConn,
	interfaces []net.Interface,
	query []byte,
	destination *net.UDPAddr,
) error {
	var lastErr error
	sent := false
	for index := range interfaces {
		if err := conn.SetMulticastInterface(&interfaces[index]); err != nil {
			lastErr = err
			continue
		}
		if _, err := conn.WriteTo(query, nil, destination); err != nil {
			lastErr = err
			continue
		}
		sent = true
	}
	if sent {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no discovery interfaces")
}

func mergeDiscoveryResponse(
	info *channelInfo,
	packet []byte,
	source net.IP,
	requested string,
	requiredSource net.IP,
) (bool, error) {
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
		info.address = append(net.IP(nil), source...)
	}
	if port != 0 {
		info.port = port
		info.hasSRV = true
	}
	if txt != nil {
		address, savedPort, hasSRV := info.address, info.port, info.hasSRV
		*info = *txt
		info.address, info.port, info.hasSRV = address, savedPort, hasSRV
	}
	return info.hasSRV && info.hasTXT, nil
}
