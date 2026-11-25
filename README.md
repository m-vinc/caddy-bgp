# caddy-bgp

A Caddy app that embeds [GoBGP](https://github.com/osrg/gobgp) to announce anycast
prefixes while the node is healthy. It only announces: learned routes are rejected
and the kernel routing table is never touched.

## Example

```caddyfile
{
	bgp {
		local_as 65000
		router_id 192.0.2.10

		peer 192.0.2.1 {
			remote_as 64500
		}

		announce web 198.51.100.10/32 {
			check http http://127.0.0.1:9180/healthz
		}
	}
}

http://127.0.0.1:9180 {
	respond /healthz 200
}

http://198.51.100.10 {
	reverse_proxy 10.0.0.2:8080 10.0.0.3:8080
}
```

The node peers with its router and announces `198.51.100.10/32` once
`/healthz` answered twice in a row. It withdraws the route when the check fails
twice or when Caddy stops, and reloads keep the session up.

Every option, the admin API and the metrics are in
[docs/reference.md](docs/reference.md).

## Build

    xcaddy build --with github.com/m-vinc/caddy-bgp

## Develop

    task check-style
    task test
