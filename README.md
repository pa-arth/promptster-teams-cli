# promptster-teams-cli has moved

**Source, issues and releases now live at
[promptster-ai/promptster-teams-cli](https://github.com/promptster-ai/promptster-teams-cli).**

This repository exists only so that installs from before the move can update
themselves. Those builds look for updates here; the bridge release in this
repository is the same signed build published at the new home, byte for byte.
Once a machine updates to it, it reads updates from `promptster-ai` and never
returns here.

Install or upgrade:

```sh
npm i -g @promptster/teams-cli
# or
curl -fsSL https://raw.githubusercontent.com/promptster-ai/promptster-teams-cli/main/install.sh | sh
```

`install.sh` is kept here too, for older instructions that still point at this
repository. It installs from `promptster-ai`.
