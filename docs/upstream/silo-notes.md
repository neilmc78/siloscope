# Notes for Silo maintainers

Two findings from building Siloscope and its Silo plugin. They're written up so they can be filed
upstream as issues. Both refer to silo-server as of November 2026; file paths are relative to that
repository.

## 1. Anyone can lock an account out of password sign-in

### What happens

Silo limits password sign-in per sign-in name: 10 attempts in 15 minutes, then the name is locked
for 15 minutes (`internal/ratelimit/attempts.go`, `SignInNamePolicy`). While it's locked, **every
attempt is refused, the correct password included**. The budget is spent before any password
check, whether or not the account exists (`internal/auth/signin_attempts.go`). Every password path
goes through the same `auth.Service` budget: the web app, the native apps, Jellyfin clients
(`internal/jellycompat/login.go` handles `SignInThrottledError`) and the Audiobookshelf protocol.

The limit stops a stranger guessing a password, which is its purpose. It also hands that stranger
a cheap way to stop the real person signing in. Ten wrong passwords every 15 minutes keep a name
locked indefinitely, and they can come from anywhere, at about 40 requests an hour.

### Reproduce

```sh
for i in $(seq 1 10); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST "$SILO/api/v2/auth/login" \
    -H 'Content-Type: application/json' -d '{"username":"admin","password":"wrong"}'
done
# The real password is now refused for 15 minutes:
curl -s -X POST "$SILO/api/v2/auth/login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<the real password>"}'
```

### Impact

- On an internet-facing server (port forward, reverse proxy, Tailscale Funnel), anyone who knows or
  guesses an admin's sign-in name can keep that admin out of every password sign-in, on every
  client, for as long as they like.
- Devices that are already signed in keep working: refresh tokens aren't affected. New sign-ins
  fail, and that includes recovery after a reinstall, a new TV app, or a password manager filling
  the login on a new device.
- Break-glass accounts, the Owner by default, are meant to be the always-available way back in, and
  they're locked the same way.

### "Don't use `admin` as the sign-in name"

That's good advice, and Silo helps by keeping `/Users/Public` empty, so usernames aren't listed.
It only partly mitigates the problem:

- Many Owner accounts are `admin`, the household's surname, or the owner's email address. Emails
  count as sign-in names too.
- Usernames show up in places other people see: shared profile pickers, invitation emails, and
  logs or screenshots posted for support.
- Anything that relays sign-in to Silo (companion tools, dashboards, scripts) passes on the
  attacker's attempts. Siloscope now refuses a name after 5 failures so it can never trip Silo's
  10, but other tools may not.

### Suggested fixes, smallest first

1. **Lock per name and source**, not per name alone. Keep a per-name ceiling, but make it much
   higher, or a slow-down rather than a lock. One source then can't lock the owner out from
   everywhere.
2. **Let a known device through.** A sign-in from a device that holds a valid, recently used refresh
   token or device ID for that account skips the per-name lock. The person the lock protects is
   rarely the person it blocks.
3. **Progressive delay instead of a hard lock**: each failure adds a few seconds, without refusing
   the correct password outright.
4. **Make the policy configurable** (attempts, window, lock or delay) under Sign-in & Security, and
   show locked names to admins with a "Clear lock" action.
5. **Exempt break-glass accounts from the hard lock**, or give them a separate, much higher budget,
   since their whole job is to be available when everything else fails.

## 2. "Check Connection" for plugins that aren't metadata or marker providers

The admin plugin form shows **Check Connection** for every plugin that has settings. Silo only
runs a check for `metadata_provider.v1` and `marker_provider.v1` capabilities
(`internal/plugins/service_connection.go`, `connectionCheckCapability`). Every other plugin
answers "Connection checks are not supported for this plugin yet." That includes plugins that only
use `http_routes.v1`, such as the Siloscope sidebar link.

A plugin can't fix this itself: the only way to pass is to declare a marker provider, and Silo would
then use the plugin for intro and credit markers.

Suggested fix: an optional generic RPC, for example `Runtime.TestConfig(ConfigureRequest) returns
(TestConfigResponse{success, message})`. Silo would call it for any plugin that implements it and
fall back to today's message when it returns `Unimplemented`. Alternatively, hide the button for
plugins that can't be checked. For the Siloscope plugin, the check would confirm that the URL
answers and that the launch secret matches.
