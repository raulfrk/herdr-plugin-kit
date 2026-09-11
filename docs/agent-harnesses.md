# Agent harness installation

The `herdr-plugin-development` skill teaches coding agents to generate,
customize, validate, test, and review Go plugins built with Herdr Plugin Kit.
The canonical skill is `skills/herdr-plugin-development`; every supported
harness loads that same directory.

## Codex

The Codex plugin described by `.codex-plugin/plugin.json` exposes only the
canonical skill. A marketplace can catalogue the checkout as the
`herdr-plugin-development` plugin. For a direct skill installation, ask Codex's
`$skill-installer` to install this repository's
`skills/herdr-plugin-development` directory.

To expose a trusted local checkout directly, link its canonical directory into
the active Codex home:

```sh
mkdir -p "${CODEX_HOME:-$HOME/.codex}/skills"
ln -s /absolute/path/to/herdr-plugin-kit/skills/herdr-plugin-development \
  "${CODEX_HOME:-$HOME/.codex}/skills/herdr-plugin-development"
```

Invoke it explicitly as `$herdr-plugin-development`, or ask Codex to create,
modify, validate, test, or review a Herdr plugin.

## OpenCode

OpenCode discovers project skills from `.agents/skills`. This repository exposes
the canonical skill there for development. To make it available globally, copy
or link `skills/herdr-plugin-development` from a trusted checkout to:

```text
~/.config/opencode/skills/herdr-plugin-development
```

Confirm discovery from the target project with:

```sh
opencode debug skill
```

OpenCode may load the skill automatically from its description. It can also be
selected through the native skill tool.

## Pi

`package.json` declares the canonical directory as a Pi package skill. Test or
use a trusted local checkout with:

```sh
pi install /absolute/path/to/herdr-plugin-kit
pi list
```

After a release containing this package metadata is tagged, install that exact
Git ref rather than an advancing branch:

```sh
pi install git:github.com/raulfrk/herdr-plugin-kit@<tag>
```

Pi exposes installed skills as `/skill:<name>` commands when skill commands are
enabled. It may also select the skill from its description.

## Trust and versioning

Review skills before installing them because they instruct agents that can read,
write, and run commands. Pin a Git tag for repeatable installation. The plugin
package has its own version; its instructions currently target Herdr Plugin Kit
`v0.2.0` and Herdr `0.8.2`.
