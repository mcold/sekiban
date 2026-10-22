# Building sekiban

How the program gets built, installed and released. The short version, if you
already know the drill:

```
git checkout main && git pull --ff-only origin main && go install .
```

## The branches

| Branch | What lives there |
| --- | --- |
| `develop` | the integration branch; every feature lands here first |
| `main` | the release branch; only merged, tested code |

**`main` is what gets built and installed.** Nothing is built from a feature
worktree: a binary built from `develop` is a build of work in progress, and a
binary built from a feature branch is a build of one author's idea.

The layout on disk, so the paths below are not mysterious:

```
sr-dash/src/master/     the worktree on `main`
sr-dash/src/develop/    the worktree on `develop`
sr-dash/src/<feature>/  a worktree on `feature/<name>`
```

## After a merge into main

Work arrives in `main` as a merge. Then:

```
git checkout main
git pull --ff-only origin main
```

`--ff-only` is deliberate: `main` is a release branch, and a merge commit
that nobody meant to make is cheaper to refuse than to unpick later.

### 1. Check the tree

A release is built from a tree that is clean and that passes its own checks.
Never from a dirty one — the binary then holds whatever was in the editor.

```
git status --short          # must be empty
gofmt -l .                  # must print nothing
go vet ./...
go test -count=1 ./...
```

The tests are headless by design: no display, no database, no network. The
interface test that needs a live server is behind a build tag and does not
run here.

### 2. Build

```
go build -o sekiban .
```

One binary. No cgo, no C toolchain, no bundling of native libraries — the
window is the operating system's own webview, reached at runtime.

### 3. Install it

```
go install .
```

`go install` puts the binary in `$(go env GOBIN)`, or in `$(go env GOPATH)/bin`
when `GOBIN` is unset — `~/go/bin` for a normal user. That directory is often
not on `PATH`:

```
export PATH="$(go env GOPATH)/bin:$PATH"
```

To install somewhere else instead, name the directory:

```
GOBIN=~/.local/bin go install .
```

`~/.local/bin` is a good choice on a desktop: it is already on `PATH`, and it
holds the sibling program, keikiban, the same way.

Then check that it is the binary you think it is:

```
which sekiban
sekiban -h
```

### 4. Smoke it

```
sekiban -json connections
```

It answers with JSON and exits, or with the connection error that says what is
wrong. A real query needs a configured connection — see the README for the
config file.

## A note on `go install github.com/...@version`

This does **not** work for this repository, and the failure is worth knowing
about:

```
$ go install github.com/mcold/sekiban@latest
go: github.com/mcold/sekiban@latest: version constraints conflict:
	module declares its path as: github.com/crgimenes/sekiban
	        but was required as: github.com/mcold/sekiban
```

The module still declares the upstream path, `github.com/crgimenes/sekiban`,
because it is a fork of it and the import path is part of that lineage. Go
resolves a module by its declared path, so the fork cannot be installed by
import path.

So clone and install from the checkout:

```
git clone https://github.com/mcold/sekiban.git
cd sekiban
git checkout main
go install .
```

If the module path is ever changed to `github.com/mcold/sekiban` — one line in
`go.mod`, and the only thing standing between this repository and a
`go install github.com/mcold/sekiban@latest` that needs no clone at all — then
the command above works, and this note goes away.

## Cross-compiling

There is no C cross-compiler in the way, so every target is one `go build`
away. The release targets are the ones CI builds:

```
mkdir -p dist
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
    GOOS=${target%/*} GOARCH=${target#*/} go build -o "dist/sekiban-${target%/*}-${target#*/}" .
done
```

`windows/386` is not a target: glaze's WebView2 layer has no 386 shim.

The macOS binary is unsigned, so Gatekeeper complains on first run:

```
xattr -dr com.apple.quarantine sekiban-darwin-arm64
```

Windows SmartScreen may want "More info" and then "Run anyway".

## What the machine running it needs

The binary binds the webview the operating system already provides, so
nothing native is bundled — but that webview has to be there.

| System | Needs |
| --- | --- |
| macOS | nothing; WKWebView ships with the OS |
| Windows | the Edge WebView2 runtime, preinstalled on current Windows 10 and 11 |
| Linux | a system WebKitGTK, GTK4 or GTK3 — glaze detects which at runtime |

On Linux:

```
apt install libwebkit2gtk-4.1-0      # Debian / Ubuntu, GTK3
apt install libwebkitgtk-6.0-4       # Debian / Ubuntu, GTK4
dnf install webkit2gtk4.1            # Fedora, GTK3
dnf install webkitgtk6.0             # Fedora, GTK4
```

The two stacks must not be mixed: glaze loads GTK4 only when
`libwebkitgtk-6.0.so.4` is present, and never loads both, because a process
with GTK3 and GTK4 in it corrupts GObject's type system and crashes at
`gtk_init`.

To see what the loader can actually find:

```
ldconfig -p | grep -E 'libwebkit(2)?gtk|libjavascriptcoregtk|libgtk-[34]'
```

## Releasing

A release is a tag on `main`.

```
git tag -a v0.1.0 -m "sekiban v0.1.0"
git push origin v0.1.0
```

The tag names the tree that was built. Build from the tag, not from the
branch, so the binary and the tag cannot drift apart:

```
git checkout v0.1.0
go build -o sekiban .
```

## One command

For the common case — sync `main`, install, done:

```
git -C /path/to/sr-dash/src/master checkout main \
  && git -C /path/to/sr-dash/src/master pull --ff-only origin main \
  && (cd /path/to/sr-dash/src/master && go install .)
```

## The interface, without a database

The UI is plain HTML, JavaScript and canvas. It can be opened in a plain
browser, with recorded data and no cluster behind it:

```
sh tools/uimock/serve.sh
```

The recordings are real captures from a live cluster. Re-record them with
`sh tools/record_demo.sh`; a chart drawn from a synthetic curve would be a
picture of nothing.
