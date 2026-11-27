# uefimgr

A Windows app for managing UEFI boot entries. It edits the firmware's boot
variables directly: the boot order, the entries, the one-time next boot, the
menu timeout and the request to open firmware setup.

You edit a staged copy. Nothing reaches the firmware until you review the
writes and confirm.

## Status

Tested on Windows x64. The ARM64 build compiles but has never run.

Firmware differs between vendors, and a bad write to the boot variables can
leave a machine unable to start. Keep a backup, and know how to reach your
firmware's boot menu before you write anything.

## Requirements

- Windows 10 or 11, x64 or ARM64, booted in UEFI mode from a GPT disk
- An administrator account
- The WebView2 runtime, which Windows 11 and most Windows 10 installs have.
  Without it, uefimgr opens in your browser.

The executable is unsigned, so SmartScreen may warn about it.

## Using it

Run `uefimgr.exe` and accept the UAC prompt.

The main list is the boot order. The firmware tries entries from the top.
For each entry you can reorder it, enable or disable it, set it as the next
boot, edit its name, loader or partition, or delete it.

"New entry" lists EFI system partitions and other FAT volumes on GPT disks
and lets you browse them for `.efi` files.

Entries that exist but aren't in the boot order show in a separate list.

"Review and write" shows every variable that will change. Before the first
write, uefimgr saves a backup and writes nothing if the backup fails. After
writing, it reads everything back and reports any differences.

uefimgr asks for extra confirmation if a change would leave no bootable
entry, or would remove or break the loader this system started from.

### Backups

Backups are JSON files in `%ProgramData%\uefimgr\backups`. uefimgr keeps the
50 newest automatic ones and never deletes manual ones. Loading a backup
stages it like any other edit.

To restore without the window, for example from a recovery prompt:

```bat
uefimgr.exe restore C:\path\to\backup.json
```

### Troubleshooting

- `uefimgr.exe --browser` opens the UI in your browser
- The log is at `%LOCALAPPDATA%\uefimgr\uefimgr.log`
- `UEFIMGR_DEBUG=1` enables developer tools in the window

## Trying it without Windows

```sh
make demo
```

This runs the UI in your browser against a JSON file with sample entries.

## Building

Go 1.22 or newer, no cgo.

```sh
make windows   # dist/uefimgr.exe and dist/uefimgr-arm64.exe
make release VERSION=v0.1.0   # same, plus dist/SHA256SUMS
make test
make vet
```

On Windows, use `.\build.ps1` or `.\build.ps1 -Arch arm64`.

The icon and manifest live in `cmd/uefimgr/winres/`. After changing them,
run `make icon`, which needs Chrome and `go-winres`.

Dependencies are `golang.org/x/sys` and `github.com/wailsapp/go-webview2`.

## How it works

uefimgr reads and writes `BootOrder`, `Boot####`, `BootNext`, `Timeout` and
`OsIndications` in the EFI global namespace through the Win32 firmware
variable API.

New entries get a single `HD(...)/FilePath(...)` device path built from the
partition's GPT info. Editing an entry keeps everything you didn't change
byte for byte. uefimgr can move, delete and back up entries it can't
decode, but not edit them.

Writes go in this order: new and changed entries, then `BootOrder`, then
deletions, then everything else. If a write fails partway, `BootOrder` still
names only entries that exist.

## Layout

```
cmd/uefimgr/        entry point
internal/efi/       device path and load option codecs
internal/store/     firmware, JSON file and staging stores
internal/manager/   edits, risk checks, backups
internal/esp/       partition discovery and file browsing
internal/target/    partition choice and device path for an entry
internal/ui/        page, API, HTTP
internal/gui/       WebView2 window, message boxes
internal/demo/      sample data
```

## Not done

- Code signing
- Per-monitor DPI
- BCD store, ESP backup, Secure Boot keys
- MBR disks and legacy BIOS

## License

MIT
