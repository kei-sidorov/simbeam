# Deploying simcast remote access (Phase 3b)

> **Deploy-only.** None of this is exercised by the repo's local tests. Local
> validation (see the plan's Task 9) proves the *functional* pairing flow over
> localhost (host candidates only). Real NAT traversal, `srflx`/`relay`
> candidates, and a running coturn require this deployment.

## Components

1. **Signaling broker** — `cmd/simcast-signal`. Stateless WSS rendezvous.
2. **coturn** — off-the-shelf TURN relay. We only configure it (`coturn/turnserver.conf`).
3. **Daemon** — `simcastd serve --signal wss://<broker-host>/ws --web ./web/debug`.

## Broker

```bash
go build -o simcast-signal ./cmd/simcast-signal
./simcast-signal \
  --addr :9000 \
  --stun stun:<stun-host>:3478 \
  --turn turn:<turn-host>:3478 \
  --turn-secret "<SAME-AS-COTURN-static-auth-secret>" \
  --turn-ttl 1m \
  --grant-turn=false   # STUB gate; true grants TURN to every room (dev only)
```

Put the broker behind TLS (`wss://`) in production — terminate TLS at a reverse
proxy or extend the broker. The pairing URL embeds `wss://`.

## coturn

Install coturn (`apt install coturn` / `brew install coturn`), set
`static-auth-secret` in `coturn/turnserver.conf` to the **same value** as the
broker's `--turn-secret`, set `external-ip`, and run `turnserver -c turnserver.conf`.

## Subscription gating (Phase 4)

`--grant-turn` is a STUB: it grants TURN to all rooms or none. Real per-user
billing/subscription checks (`GrantTURN(room)` keyed to an account) are Phase 4.

## ICE entries the browser receives

| Entry | When | Cost |
|-------|------|------|
| `stun:` | always | ~free (stateless) |
| `turn:` + ephemeral HMAC creds | only when `GrantTURN(room)` is true | relays media — the metered resource |

Free tier (STUN only): works on the same LAN and on friendly NATs; a hostile
symmetric NAT yields `iceConnectionState === "failed"` and the client shows the
upsell.
