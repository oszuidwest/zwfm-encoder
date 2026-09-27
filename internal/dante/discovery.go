package dante

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const channelService = "_netaudio-chan._udp"

type advertisedChannel struct {
	address         *net.UDPAddr
	ID              uint16
	channelsPerFlow int
	bitsPerSample   int
	sampleRate      int
	dbcp1           uint16
	fppMin          int
	fppMax          int
}

func resolveChannels(ctx context.Context, config *Config) ([2]advertisedChannel, net.IP, error) {
	var channels [2]advertisedChannel

	iface, localIP, err := selectInterface(config.Interface)
	if err != nil {
		return channels, nil, err
	}

	resolver, err := zeroconf.NewResolver(
		zeroconf.SelectIPTraffic(zeroconf.IPv4),
		zeroconf.SelectIfaces([]net.Interface{*iface}),
	)
	if err != nil {
		return channels, nil, fmt.Errorf("create Dante mDNS resolver: %w", err)
	}

	for i, name := range config.Channels {
		channel, resolveErr := resolveChannel(ctx, resolver, config.Transmitter, name)
		if resolveErr != nil {
			return channels, nil, resolveErr
		}
		channels[i] = channel
	}

	if err := validateChannelPair(&channels); err != nil {
		return channels, nil, err
	}
	return channels, localIP, nil
}

func resolveChannel(
	ctx context.Context,
	resolver *zeroconf.Resolver,
	transmitter string,
	channelName string,
) (advertisedChannel, error) {
	var channel advertisedChannel
	instance := channelName + "@" + transmitter
	lookupCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry, 1)
	if err := resolver.Lookup(lookupCtx, instance, channelService, "local.", entries); err != nil {
		return channel, fmt.Errorf("resolve Dante channel %q: %w", instance, err)
	}

	select {
	case <-lookupCtx.Done():
		return channel, fmt.Errorf("resolve Dante channel %q: %w", instance, lookupCtx.Err())
	case entry, ok := <-entries:
		if !ok || entry == nil {
			return channel, fmt.Errorf("resolve Dante channel %q: no mDNS answer", instance)
		}
		parsed, err := parseChannelEntry(entry)
		if err != nil {
			return channel, fmt.Errorf("resolve Dante channel %q: %w", instance, err)
		}
		return parsed, nil
	}
}

func parseChannelEntry(entry *zeroconf.ServiceEntry) (advertisedChannel, error) {
	properties := make(map[string]string, len(entry.Text))
	for _, text := range entry.Text {
		key, value, ok := strings.Cut(text, "=")
		if ok {
			properties[key] = value
		}
	}

	if len(entry.AddrIPv4) == 0 {
		return advertisedChannel{}, errors.New("mDNS answer has no IPv4 address")
	}

	channelID, err := parseTXTInt(properties, "id")
	if err != nil {
		return advertisedChannel{}, err
	}
	channelsPerFlow, err := parseTXTInt(properties, "nchan")
	if err != nil {
		return advertisedChannel{}, err
	}
	bitsPerSample, err := parseTXTIntFallback(properties, "enc", "en")
	if err != nil {
		return advertisedChannel{}, err
	}
	sampleRate, err := parseTXTInt(properties, "rate")
	if err != nil {
		return advertisedChannel{}, err
	}
	dbcp1, err := parseTXTInt(properties, "dbcp1")
	if err != nil {
		return advertisedChannel{}, err
	}
	fppMax, fppMin, err := parseFPP(properties["fpp"])
	if err != nil {
		return advertisedChannel{}, err
	}
	if channelID > int(^uint16(0)) || dbcp1 > int(^uint16(0)) {
		return advertisedChannel{}, errors.New("dante mDNS channel identifiers exceed uint16")
	}

	return advertisedChannel{
		address: &net.UDPAddr{
			IP:   entry.AddrIPv4[0].To4(),
			Port: entry.Port,
		},
		ID:              uint16(channelID), //nolint:gosec // checked immediately above.
		channelsPerFlow: channelsPerFlow,
		bitsPerSample:   bitsPerSample,
		sampleRate:      sampleRate,
		dbcp1:           uint16(dbcp1), //nolint:gosec // checked immediately above.
		fppMin:          fppMin,
		fppMax:          fppMax,
	}, nil
}

func parseTXTInt(properties map[string]string, key string) (int, error) {
	value, ok := properties[key]
	if !ok {
		return 0, fmt.Errorf("dante mDNS property %q is missing", key)
	}
	parsed, err := strconv.ParseUint(value, 0, 32)
	if err != nil {
		return 0, fmt.Errorf("parse Dante mDNS property %s=%q: %w", key, value, err)
	}
	return int(parsed), nil
}

func parseTXTIntFallback(properties map[string]string, keys ...string) (int, error) {
	for _, key := range keys {
		if _, ok := properties[key]; ok {
			return parseTXTInt(properties, key)
		}
	}
	return 0, fmt.Errorf("dante mDNS properties %q are missing", keys)
}

func parseFPP(value string) (maximum, minimum int, err error) {
	maximumText, minimumText, ok := strings.Cut(value, ",")
	if !ok {
		return 0, 0, fmt.Errorf("invalid Dante fpp property %q", value)
	}
	maximum, err = strconv.Atoi(maximumText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse Dante maximum fpp %q: %w", maximumText, err)
	}
	minimum, err = strconv.Atoi(minimumText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse Dante minimum fpp %q: %w", minimumText, err)
	}
	return maximum, minimum, nil
}

func validateChannelPair(channels *[2]advertisedChannel) error {
	left := channels[0]
	right := channels[1]
	sameTransmitter := left.address.IP.Equal(right.address.IP) && left.address.Port == right.address.Port
	compatibleFormat := left.bitsPerSample == right.bitsPerSample && left.sampleRate == right.sampleRate
	compatibleProtocol := left.dbcp1 == right.dbcp1 && left.channelsPerFlow == right.channelsPerFlow
	if !sameTransmitter || !compatibleFormat || !compatibleProtocol {
		return errors.New("dante channels do not belong to one compatible transmitter flow")
	}
	if left.sampleRate != sampleRate {
		return fmt.Errorf("dante transmitter uses %d Hz; encoder requires %d Hz", left.sampleRate, sampleRate)
	}
	if left.channelsPerFlow < 2 {
		return fmt.Errorf("dante transmitter supports only %d channel per flow", left.channelsPerFlow)
	}
	if left.bitsPerSample != 16 && left.bitsPerSample != 24 && left.bitsPerSample != 32 {
		return fmt.Errorf("unsupported Dante sample depth %d", left.bitsPerSample)
	}
	return nil
}

func selectInterface(name string) (*net.Interface, net.IP, error) {
	if name != "" {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return nil, nil, fmt.Errorf("find Dante interface %q: %w", name, err)
		}
		ip, err := interfaceIPv4(iface)
		if err != nil {
			return nil, nil, err
		}
		return iface, ip, nil
	}

	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
	if err != nil {
		return nil, nil, fmt.Errorf("select Dante interface: %w", err)
	}
	localIP := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	_ = conn.Close()

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for i := range interfaces {
		addresses, addressErr := interfaces[i].Addrs()
		if addressErr != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, parseErr := net.ParseCIDR(address.String())
			if parseErr == nil && ip.Equal(localIP) {
				return &interfaces[i], localIP, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("no interface owns Dante source IP %s", localIP)
}

func interfaceIPv4(iface *net.Interface) (net.IP, error) {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return nil, fmt.Errorf("dante interface %q must be up and multicast-capable", iface.Name)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("list addresses for Dante interface %q: %w", iface.Name, err)
	}
	for _, address := range addresses {
		ip, _, parseErr := net.ParseCIDR(address.String())
		if parseErr == nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.To4(), nil
		}
	}
	return nil, fmt.Errorf("dante interface %q has no usable IPv4 address", iface.Name)
}
