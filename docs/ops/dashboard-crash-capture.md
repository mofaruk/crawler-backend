# Dashboard crash loop — what is known, and how to capture the next one

The production dashboard (Coolify resource `mofaruk/crawler-dashboard` on
`vmi3025535`) periodically enters a crash loop. Coolify shows **"Restart limit
reached"**, the site answers Traefik's bare `404 page not found`, and a manual
redeploy restores it. Seen 2026-09-09 and 2026-09-11 (~20:30 UTC). The cause
is **not known**. This file exists so the next occurrence produces evidence
instead of another guess.

## What "Restart limit reached" means

The container *process* is exiting repeatedly and Docker's restart policy is
restarting it. It is not the healthcheck: Coolify does not restart unhealthy
containers, and the compose healthcheck is confirmed present and passing in
the production image (its `GET /index.php` every 10s is visible in the logs).

## Theories already ruled out — do not re-investigate

| Theory | Why it is dead |
|---|---|
| Missing `restart:` policy | Coolify hard-codes one |
| Entrypoint `set -e` failing at boot | Runtime logs were empty; boot completes |
| Full disk | 45% used |
| `healthcheck.sh` missing from the image | Was a stale *local* image (built Aug 28); prod has it |
| OOM kill | `dmesg -T \| grep -i oom` empty on 2026-09-11 |
| Redis/deploy orphaning | That was crawls, not the dashboard |

## Why there was never a log

`laravel.log` lives in `storage/logs` inside the container — no volume — and
dies with it. Coolify removes the crashed container on redeploy, taking
`docker logs` and the exit code with it. Every inspection so far was run
*after* the redeploy, on the new, healthy container. A follower started on
2026-09-09 (`nohup docker logs -f <container> > /root/dash.log`) was bound to
one container ID and stopped at the next redeploy.

## Capture now in place (since 2026-09-11)

Two `nohup` processes on the server, started by `/root/crawler-dash-watch.sh`:

```
/root/crawler-dash-events.log   docker events for crawler_dashboard*: die / oom / kill,
                                with exit code and signal. Permanent. Empty until a death.
/root/crawler-dash.log          docker logs -f follower that re-attaches to each new
                                container and APPENDS, so it spans redeploys.
```

Check they are alive:

```
pgrep -af 'docker events|docker logs' | cut -c1-50     # expect 3 lines
tail -2 /root/crawler-dash.log                          # expect fresh nginx lines
```

Caveats:

- They are `nohup`, not systemd: a server reboot ends them silently. Re-run
  `/root/crawler-dash-watch.sh` afterwards — **but its saved second line is
  broken by a terminal wrap**; run the two one-liners below instead, or fix
  the file first.
- Both must use `>>`. A `>` truncates the log at exactly the moment it matters.

The two commands, each on one unbroken line:

```
nohup docker events --filter name=crawler_dashboard --filter event=die --filter event=oom --filter event=kill --format '{{.Time}} {{.Actor.Attributes.name}} {{.Action}} exit={{.Actor.Attributes.exitCode}} signal={{.Actor.Attributes.signal}}' >> /root/crawler-dash-events.log 2>&1 &
nohup sh -c 'while :; do C=$(docker ps -q --filter name=crawler_dashboard- | head -1); [ -n "$C" ] && docker logs -f --timestamps "$C" >> /root/crawler-dash.log 2>&1; sleep 5; done' >/dev/null 2>&1 &
```

## At the next occurrence — BEFORE redeploying

```
cat /root/crawler-dash-events.log
tail -40 /root/crawler-dash.log
C=$(docker ps -aq --filter name=crawler_dashboard | head -1)
docker inspect --format 'exit={{.State.ExitCode}} oom={{.State.OOMKilled}} restarts={{.RestartCount}} err={{.State.Error}}' $C
dmesg -T | grep -iE 'oom|killed process' | tail -5
```

The exit code alone splits the possibilities:

- `137` / `signal=9` — killed from outside (kernel, Docker, Coolify)
- `1` — the entrypoint failed (`migrate --seed`, cache build, permissions)
- `0` — something asked it to stop

Only then redeploy.

## Not yet done

- `LOG_CHANNEL=stderr` in the dashboard compose, so Laravel's own log reaches
  `docker logs` and this capture. Cheap; catches the PHP-fatal case.
- A systemd unit for the two watchers, so they survive a reboot.
- A volume for `storage/` — also loses uploaded CSVs on every deploy.
