# pallium

NPM installer for the Pallium CLI.

```bash
npm install -g pallium
pallium version
```

The package installs the matching Pallium release binary from GitHub into
`~/.pallium/npm/<version>/`. If a prebuilt binary is unavailable for the current
platform, it falls back to:

```bash
go install github.com/tszaks/pallium@v0.9.3
```

GitHub Packages mirror:

```bash
npm config set @tszaks:registry https://npm.pkg.github.com
npm install -g @tszaks/pallium
```

Supported prebuilt platforms:

- macOS arm64
- macOS x64
- Linux arm64
- Linux x64

Pallium itself stores local data in `~/.pallium/` and repo-local `.pallium/`
databases.
