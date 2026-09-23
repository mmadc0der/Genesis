# SSH client check

This checks that the Genesis image has a client `ssh` and that the root-only
secret directory is not readable by the listener user. It does not contact
GitHub, register a deploy key, or read a private key.

Run it from the repository root on the WSL filesystem, with Docker Desktop
integration enabled for that distro. Do not run it from PowerShell or
`cmd.exe`.

```sh
sh scripts/wsl-docker-ssh-check.sh
```

The script refuses to start when `GITHUB_TOKEN`, `GH_TOKEN`, or an App
private-key variable is set. It does not delete volumes and it does not run
`ssh` against `github.com`. `ssh -V` and a listener-user permission check are
the whole container exercise.

Successful output includes the OpenSSH client version and
`ssh client check passed`. An SSH server binary, a readable secrets
directory, or a missing `ssh` fails the script.

Repository creation, rulesets, Actions, webhooks, and App-token delivery are
not part of this check.
