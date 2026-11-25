# Reference

## Global option

```caddyfile
{
	bgp {
		local_as <asn>
		router_id <IPv4>
		listen <address...>
		port <port>
		interface <name>
		drain_delay <duration>

		peer <address> {
			remote_as <asn>
			description <text>
			local_address <address>
			port <port>
			families ipv4 ipv6
			password <text>
			passive
			optional
			multihop <ttl>
			ttl_security <hops>
			timers <keepalive> <hold>
			connect_retry <duration>
			max_prefixes <count>
			bfd [<interval> [<multiplier>]] {
				multihop
			}
		}

		announce <name> [<prefix...>] {
			prefix <prefix...>
			next_hop <address...>
			community <value...>
			med <number>
			prepend <count>
			always
			hold_down <duration> [<maximum>]
			flap_window <duration>
			check <probe> <arguments...> {
				interval <duration>
				timeout <duration>
				rise <count>
				fall <count>
				<probe options>
			}
		}
	}
}
```

Placeholders (`{env.NAME}`, `{file./path}`, `{system.hostname}`) are resolved when
the configuration loads, in `local_as`, `router_id`, `listen`, `port`, the peer
`address`, `remote_as`, `local_address`, `port` and `password`, the prefixes, next
hops, communities and the probe URL. An unknown placeholder is an error. `{$NAME}`
also works everywhere in a Caddyfile, at parse time.

| Option | Default | Meaning |
| --- | --- | --- |
| `local_as`, `router_id` | required | The speaker. Changing them, `listen` or `port` resets every session. |
| `listen` | none | Addresses to accept sessions on. Without it there is no listener and the node opens every session. |
| `port` | 179 | Port of the listener. |
| `interface` | none | Manage the host addresses of the announcements on this interface (see below). |
| `drain_delay` | 5s | Wait between the withdrawal and the closing of the listeners on shutdown, 0 to skip. |

### Peers

| Option | Default | Meaning |
| --- | --- | --- |
| `families` | the family of the address | Address families of the session. |
| `local_address` | chosen by the kernel | Source address. Set it when the node holds several addresses of the family. |
| `port` | 179 | Port of the peer. |
| `password` | none | TCP MD5 password, at most 80 bytes. Use `{file./run/secrets/bgp}` or `{env.NAME}` so that it stays out of the configuration shown by the admin API. |
| `passive` | off | Wait for the peer to connect, needs `listen`. |
| `optional` | off | Ignore the peer when its address resolves to an empty string. |
| `multihop` | off | TTL for an external peer that is not directly connected. |
| `ttl_security` | off | Refuse packets that crossed more than this number of hops (GTSM), 1 for a direct peer. Both sides need it. |
| `timers` | `3s 9s` | Keepalive and hold time, in whole seconds. |
| `connect_retry` | 5s | Time between connection attempts, at least 2s. |
| `max_prefixes` | none | Close the session when the peer sends more. GoBGP does not bring it back by itself. |
| `bfd` | off | Single-hop BFD, 300ms and 3 unless given. GoBGP then binds UDP 3784 and 4784 on every address. FRR 10.5 only resets the session at once on a BFD failure when the neighbor has its own `neighbor ... timers`; with the global `timers bgp` it waits 30 s, so the hold timer wins. |

Graceful restart is never negotiated. A restarting node must lose its traffic, not
keep it through stale routes, and a reload never closes a session.

### Announcements

| Option | Default | Meaning |
| --- | --- | --- |
| `prefix` | required | Prefixes, an address alone is a host route. A default route is refused. |
| `next_hop` | the local address of the session | At most one per family. Needed when a session carries the other family. |
| `community` | none | `65000:100`, a well-known name such as `no-export`, or a large community `65000:1:2`. |
| `med` | none | Multi-exit discriminator. |
| `prepend` | 0 | How many more times the local AS appears in the path, external peers only. |
| `always` | off | Announce without a check. One of `check` or `always` is required. |
| `hold_down` | `10s 1m` | Minimum time between a withdrawal and the next announcement. It doubles with each flap up to the maximum, 0s disables it. |
| `flap_window` | 5m | Time an announcement must stay up for its flaps to be forgotten. |

Every check must be healthy. A check becomes healthy after `rise` successes in a row
(2) and unhealthy after `fall` failures in a row (2), one probe every `interval`
(5s) with a `timeout` (2s).

### Probes

```caddyfile
check http https://127.0.0.1/api/ready {
	method GET
	host app.example.com
	header X-Probe bgp
	status 200 2xx
	tls_server_name app.example.com
	tls_insecure
	tls_ca /etc/caddy/root.pem
}
```

Any 2xx answer is a success by default, redirects are not followed and every probe
opens a new connection.

```caddyfile
check server https
```

Connects to the TCP and unix listen addresses of a Caddy HTTP server, through the
loopback for a wildcard address. Servers are named with the `servers { name }`
global option, otherwise they are called `srv0`, `srv1` and so on. The kernel accepts
connections for a hung process, so prefer `http` when there is something to ask.

```caddyfile
check tcp 10.0.0.2:8080
```

Opens a TCP connection and closes it. The address is `host:port`; a name is resolved
at each probe.

```caddyfile
check icmp 10.0.0.2
```

Sends one echo request to an IPv4 or IPv6 address and waits for the reply. It uses a
ping socket when `net.ipv4.ping_group_range` allows the group of the process (the
default of systemd and of container runtimes), otherwise a raw socket, which needs
`CAP_NET_RAW`. A peer that answers only shows that the host is up, not the service.

```caddyfile
check upstreams 10.0.0.1:8080 10.0.0.2:8080 {
	server https
	min_available 1
}
```

Reads the health of the `reverse_proxy` upstreams: active checks, passive failures
and circuit breakers. Without addresses, every static upstream counts. An upstream
used by several handlers is available when it is in all of them. An upstream without
health checks is always available, and dynamic upstreams are not seen.

Other modules can add probes in the `bgp.probes` namespace: a module with a method
`Probe(context.Context) error`.

### Managed interface

With `interface cbgp0`, the app creates `cbgp0` as a dummy interface if it does not
exist and adds the host prefixes (/32 and /128) of every announcement to it, whether
they are announced or not. It needs `CAP_NET_ADMIN`.

- A link it created carries the alias `managed by caddy-bgp`. It and everything on it
  belong to the app, also after a crash, and it is deleted on shutdown.
- On any other link, only the addresses added by the running process are removed.
- An address dropped by a reload stays for `drain_delay`.
- Nothing is touched by `caddy validate` or `caddy adapt`.

Policy routing for the return path, `arp_ignore`, `arp_announce` and the like stay
with the host. Apps start in no particular order, so a site with `bind` on an anycast
address also needs `net.ipv4.ip_nonlocal_bind` (and its IPv6 twin) or an interface
that exists beforehand.

## JSON

The app is `bgp`. `local_as`, `remote_as` and `port` take numbers or strings.

```json
{
	"apps": {
		"bgp": {
			"local_as": 65000,
			"router_id": "{env.NODE_IPV4}",
			"drain_delay": "15s",
			"peers": [{"address": "192.0.2.1", "remote_as": 64500, "hold_time": "9s"}],
			"announcements": [{
				"name": "edge",
				"prefixes": ["198.51.100.100/32"],
				"checks": [{"interval": "5s", "probe": {"probe": "http", "url": "http://127.0.0.1:9180/healthz"}}]
			}]
		}
	}
}
```

[testdata/full.json](../testdata/full.json) holds every field.

## Admin API

| Request | Answer |
| --- | --- |
| `GET /bgp/peers` | state, uptime, negotiated hold time, received, imported (always 0) and advertised routes, flaps, last reason, BFD state |
| `GET /bgp/announcements` | state, prefixes, checks with their last error |
| `POST /bgp/drain` | withdraw everything, or `?announcement=<name>` |
| `POST /bgp/undrain` | announce again what is healthy |

`?wait=true` makes the drain answer after `drain_delay`. A drain survives reloads, not
a restart. States are `waiting` (never healthy yet), `announced`, `withdrawn`,
`hold_down`, `drained` and `stopping`.

The admin API can change the whole configuration: keep it on the loopback or a unix
socket, as Caddy does by default.

## Metrics

Through the registry of Caddy, on `/metrics` of the admin API or the `metrics`
handler:

| Metric | Labels |
| --- | --- |
| `caddy_bgp_peer_up` | `peer`, `remote_as`, `description` |
| `caddy_bgp_peer_flaps_total` | `peer` |
| `caddy_bgp_peer_established_timestamp_seconds` | `peer` |
| `caddy_bgp_peer_routes` | `peer`, `direction` (received, imported, advertised) |
| `caddy_bgp_announced` | `announcement`, `prefix` |
| `caddy_bgp_announcement_state` | `announcement`, `state` |
| `caddy_bgp_announcement_flaps` | `announcement` |
| `caddy_bgp_announcement_transitions_total` | `announcement`, `state` |
| `caddy_bgp_check_up` | `announcement`, `check` |
| `caddy_bgp_check_probes_total` | `announcement`, `check`, `result` |
| `caddy_bgp_check_duration_seconds` | `announcement`, `check` |

Alert on `caddy_bgp_peer_up == 0` and `caddy_bgp_announced == 0`.

## Logs

The logger is `bgp`. Peer sessions, check changes, state changes with their reason,
announcements and withdrawals are logged at info and warn. The messages of GoBGP go
to the same logger with its info level lowered to debug: raise the level of `bgp` to
debug to see why a session does not come up.

```caddyfile
log bgp {
	include bgp
	level DEBUG
}
```
