"""Adapt only Inferno's multicast DNS destination to RFC 6762 legacy unicast.

Use the mDNS source port and original response bytes. Control/media bypass this
adapter entirely. No changes to either protocol implementation are made.
"""
import socket
import struct
import time

ip = socket.gethostbyname(socket.gethostname())
listener = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(('', 5353))
listener.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP,
                    socket.inet_aton('224.0.0.251') + socket.inet_aton(ip))
queries = {}
print('Legacy reply destination adapter:', ip, flush=True)
while True:
    packet, peer = listener.recvfrom(65535)
    if len(packet) < 12:
        continue
    now = time.monotonic()
    queries = {key: expiry for key, expiry in queries.items() if expiry > now}
    if not struct.unpack_from('!H', packet, 2)[0] & 0x8000:
        if peer[1] != 5353:
            queries[peer] = now + 5
    elif peer[0] == ip:
        for destination in queries:
            listener.sendto(packet, destination)
