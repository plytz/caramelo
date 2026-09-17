package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

var examples = map[string]string{
	"caramelo app list": `
caramelo app list
caramelo app list --json`,

	"caramelo build": `
caramelo build feat-x                     # one image per service, from the env's branch head
caramelo build feat-x --ref v1.3.0        # from a tag, a branch or a commit
caramelo build feat-x --service web       # only this service
caramelo build feat-x --force             # rebuild even though this tree already has a release
caramelo build feat-x --json`,

	"caramelo config show": `
caramelo config show feat-x               # every effective field, and where each came from
caramelo config show feat-x --reveal      # with the secret values filled in
caramelo config show feat-x --json`,

	"caramelo connect": `
caramelo connect feat-x                   # a local port for every service and dependency
caramelo connect feat-x postgres          # only the database
caramelo connect feat-x web postgres --listen 0.0.0.0`,

	"caramelo deploy": `
caramelo deploy production                # build, migrate, new replicas, check, flip, watch, promote
caramelo deploy production --ref v1.3.0   # a tag, a branch or a commit instead of the branch head
caramelo deploy staging --no-watch        # return at the flip and leave the watch to the daemon
caramelo deploy production --service web  # only this service
caramelo deploy production --json --progress json   # one JSON result, one JSON event per line`,

	"caramelo down": `
caramelo down feat-x                      # services stopped; dependencies and data stay
caramelo down feat-x --service worker     # only this service
caramelo down production --force          # a protected env needs --force`,

	"caramelo edge": `
caramelo edge status
caramelo edge enable --acme-email ops@example.com
caramelo edge counts --since 10m
caramelo edge prune --dry-run`,

	"caramelo edge ca": `
caramelo edge ca > caramelo-ca.pem        # the internal CA's certificate, to trust it locally`,

	"caramelo edge counts": `
caramelo edge counts                      # requests, 5xx answers and failures per target
caramelo edge counts --since 5m --json`,

	"caramelo edge disable": `
sudo caramelo edge disable`,

	"caramelo edge enable": `
sudo caramelo edge enable --acme-email ops@example.com
sudo caramelo edge enable --tls internal               # an internal CA instead of ACME
sudo caramelo edge enable --acme-ca https://acme.example.com/dir --no-http3`,

	"caramelo edge prune": `
caramelo edge prune --dry-run             # what a prune would remove, and what it would keep
caramelo edge prune                       # stale certificates the machine's certs_keep no longer keeps
caramelo edge prune --older-than 168h     # a week instead of the machine's certs_keep
caramelo edge prune --older-than 0 --json # every stale certificate, expired or not`,

	"caramelo edge status": `
caramelo edge status                      # every route, its targets and its certificate
caramelo edge status --json               # stale certificates carry their state and issuer key`,

	"caramelo env": `
caramelo env create feat-x --from main
caramelo env list
caramelo env show feat-x
caramelo env destroy feat-x --yes`,

	"caramelo env create": `
caramelo env create feat-x                          # from the current branch
caramelo env create feat-x --from main              # from another ref
caramelo env create pr-41 --on second-box           # placed on a member of the fleet
caramelo env create production --production         # release mode, protected
caramelo env create staging --release               # release mode, not protected
caramelo env create feat-y --secrets-from feat-y.env  # its secrets from a KEY=value file, before the deps start
caramelo env create feat-x --reset --force          # start again from scratch
caramelo env create feat-x --json`,

	"caramelo env destroy": `
caramelo env destroy feat-x --yes
caramelo env destroy feat-x --yes --delete-branch
caramelo env destroy production --yes --force       # a protected env needs --force`,

	"caramelo env exec": `
caramelo env exec feat-x -- ls -la
caramelo env exec feat-x -- git log --oneline -n 5`,

	"caramelo env export": `
caramelo env export feat-x                          # the env's variables, shell format
caramelo env export feat-x --format json
caramelo env export feat-x --view network           # as seen from inside the env's network
caramelo env export feat-x --reveal                 # with secret values; recorded as an event
eval "$(caramelo env export feat-x)"`,

	"caramelo env expose": `
caramelo env expose feat-x --host demo.example.com
caramelo env expose feat-x --host api.example.com --service api
caramelo env expose pr-41 --host pr-41.example.com --via hub    # served through the hub's edge`,

	"caramelo env handoff": `
caramelo env handoff feat-x --to agent-7            # another peer now owns it`,

	"caramelo env list": `
caramelo env list                                   # this app's environments
caramelo env list --all                             # every app's
caramelo env list --mine                            # only the ones this peer owns
caramelo env list --json`,

	"caramelo env show": `
caramelo env show feat-x                            # replicas, routes, ports, addresses
caramelo env show feat-x --json`,

	"caramelo env unexpose": `
caramelo env unexpose feat-x --host demo.example.com
caramelo env unexpose feat-x                        # every name; the env keeps running`,

	"caramelo env url": `
caramelo env url feat-x                             # the app's URL
caramelo env url feat-x postgres                    # a dependency's address`,

	"caramelo events": `
caramelo events                                     # the last 100 events on the machine
caramelo events --follow                            # keep streaming
caramelo events feat-x --since 1h
caramelo events --follow --json                     # one JSON event per line`,

	"caramelo key": `
caramelo key add --name laptop --file ~/.ssh/id_ed25519.pub
caramelo key list
caramelo key remove laptop`,

	"caramelo key add": `
caramelo key add --name laptop --file ~/.ssh/id_ed25519.pub
cat agent.pub | caramelo key add --name agent-7
caramelo key add --name ci --file ci.pub --options 'restrict'`,

	"caramelo key list": `
caramelo key list
caramelo key list --json`,

	"caramelo key remove": `
caramelo key remove laptop`,

	"caramelo logs": `
caramelo logs feat-x                                # every service, merged and prefixed
caramelo logs feat-x -f                             # keep streaming
caramelo logs feat-x web --tail 100
caramelo logs feat-x --since 10m --deps             # dependencies too
caramelo logs feat-x --edge                         # the edge's access log for this env`,

	"caramelo machine": `
caramelo machine show
caramelo machine list
caramelo machine add you@second-box`,

	"caramelo machine add": `
caramelo machine add you@second-box                 # set it up and join it to this fleet
caramelo machine add you@second-box --name eu-1
caramelo machine add you@home-box --private         # no public port at all
caramelo machine add you@second-box --edge --acme-email ops@example.com
caramelo machine add you@pi --binary ./caramelo-linux-arm64   # ship a binary for another architecture
caramelo machine add you@second-box --release v0.0.1   # ship that release instead of this build`,

	"caramelo machine join": `
sudo caramelo server setup --yes                    # join needs what setup makes
sudo caramelo machine join hub.example.com:4021 --token "$(cat token)"
sudo caramelo machine join hub.example.com:4021 --token - --name eu-1 < token`,

	"caramelo machine leave": `
sudo caramelo machine leave
sudo caramelo machine leave --force                 # without the reminder to remove it on the hub too`,

	"caramelo machine list": `
caramelo machine list
caramelo machine list --json`,

	"caramelo machine remove": `
caramelo machine remove eu-1 --yes
caramelo machine remove eu-1 --yes --force          # destroying the environments it still holds`,

	"caramelo machine show": `
caramelo machine show                               # this machine
caramelo machine show eu-1                          # a member of the fleet
caramelo machine show --json`,

	"caramelo machine token": `
caramelo machine token                              # a one-time join token, printed once
caramelo machine token --ttl 1h`,

	"caramelo peer": `
caramelo peer add agent-7 <public key>
caramelo peer list
caramelo peer remove agent-7`,

	"caramelo peer add": `
caramelo peer add agent-7 Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=`,

	"caramelo peer list": `
caramelo peer list
caramelo peer list --json`,

	"caramelo peer remove": `
caramelo peer remove agent-7
caramelo peer remove agent-7 --force                # even while its tunnel is up`,

	"caramelo promote": `
caramelo promote production                         # end the watch: stop the held replicas, prune`,

	"caramelo releases": `
caramelo releases production                        # who deployed what, when, and how it ended
caramelo releases production --limit 5
caramelo releases --json`,

	"caramelo rollback": `
caramelo rollback production                        # the release before the current deploy
caramelo rollback production --to 41                # any release in the history, no build`,

	"caramelo run": `
caramelo run feat-x -- npm run lint
caramelo run feat-x -- python manage.py migrate
caramelo run feat-x --service worker -- bundle exec rake jobs:work
caramelo run feat-x --timeout 5m -- make bench`,

	"caramelo secrets": `
caramelo secrets set --app-scope STRIPE_KEY=sk_live_...
caramelo secrets set production DB_PASSWORD=chosen
caramelo secrets list production
caramelo secrets export production --reveal`,

	"caramelo secrets export": `
caramelo secrets export production                  # names only
caramelo secrets export production --reveal         # values too; recorded as an event
caramelo secrets export production --reveal --format json`,

	"caramelo secrets list": `
caramelo secrets list production                    # what this env resolves, and from which scope
caramelo secrets list --app-scope
caramelo secrets list --machine-scope`,

	"caramelo secrets rm": `
caramelo secrets rm production DB_PASSWORD
caramelo secrets rm --app-scope STRIPE_KEY`,

	"caramelo secrets set": `
caramelo secrets set production DB_PASSWORD=chosen  # before the env exists: Postgres reads it once
caramelo secrets set --app-scope STRIPE_KEY=sk_live_...
caramelo secrets set --machine-scope REGISTRY_TOKEN=...
caramelo secrets set production --from-file .env.production
printf 'STRIPE_KEY=sk_live_...' | caramelo secrets set --app-scope --stdin   # KEY=value lines or a JSON object`,

	"caramelo server": `
caramelo server setup --target you@box
caramelo server status
caramelo server probe box
sudo caramelo server uninstall --yes`,

	"caramelo server setup": `
caramelo server setup --target you@box                          # from the commander, over ssh
caramelo server setup --target you@box --edge --acme-email ops@example.com
caramelo server setup --target you@box --name prod --data-dir /mnt/big
caramelo server setup --target you@box --peer agent-7 Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=
caramelo server setup --target you@box --release v0.0.1         # ship that release, not this build
caramelo server setup --target you@box --dry-run                # say what would change
sudo caramelo server setup --yes                                # on the box itself
sudo caramelo server setup --yes --open-ports                   # let setup open udp 4021 in this box's own firewall
caramelo server setup --target you@box --json --progress json`,

	"caramelo server probe": `
caramelo server probe box                       # does its udp 4021 answer from here?
caramelo server probe 203.0.113.9:4021 --key Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=
caramelo server probe box --timeout 3s --json`,

	"caramelo server status": `
caramelo server status
caramelo server status --json`,

	"caramelo server uninstall": `
sudo caramelo server uninstall --dry-run
sudo caramelo server uninstall --yes
sudo caramelo server uninstall --yes --purge        # data and state too`,

	"caramelo status": `
caramelo status                                     # caramelod, Docker, and how we reached them
caramelo status --machine box
caramelo status --json`,

	"caramelo test": `
caramelo test feat-x                                # the app's test suite, deps wired
caramelo test feat-x -- -run TestCheckout           # arguments for the test command
caramelo test feat-x --service api --timeout 20m`,

	"caramelo up": `
caramelo up feat-x                                  # push, detect the stack, start, wait for health
caramelo up feat-x --no-wait                        # return as soon as the containers are started
caramelo up feat-x --service web                    # only this service
caramelo up feat-x --build                          # rebuild the app's own image first
caramelo up feat-x --json --progress json`,

	"caramelo version": `
caramelo version
caramelo version --json`,

	"caramelo vpn": `
caramelo vpn up
caramelo vpn status
caramelo vpn down`,

	"caramelo vpn config": `
caramelo vpn config                                 # a WireGuard config for another client`,

	"caramelo vpn down": `
caramelo vpn down`,

	"caramelo vpn install": `
sudo caramelo vpn install                           # transparent mode: names and addresses system-wide
sudo caramelo vpn install --interface wg-caramelo`,

	"caramelo vpn status": `
caramelo vpn status
caramelo vpn status --json`,

	"caramelo vpn uninstall": `
sudo caramelo vpn uninstall`,

	"caramelo vpn up": `
caramelo vpn up                                     # join the default machine's network
caramelo vpn up --machine box
caramelo vpn up --name laptop                       # the peer name the commander joins as
caramelo vpn up --transparent                       # through the installed transparent mode`,
}

func attachExamples(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if ex, ok := examples[c.CommandPath()]; ok && c.Example == "" {
			c.Example = indent(strings.TrimSpace(ex), 2)
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(root)
}
