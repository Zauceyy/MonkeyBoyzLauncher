# MonkeyBoyz Launcher

A modded Minecraft launcher that imports modpacks exported from the CurseForge app.
One exe, nothing to install. Java is downloaded automatically.

## Use it

1. Double-click `MonkeyBoyzLauncher.exe`. Windows may show "Windows protected your PC" because the
   exe is not code-signed: click **More info**, then **Run anyway**.
2. In the CurseForge app, open a profile's menu and choose **Export Profile**. You get a `.zip`.
3. Drop that zip anywhere on the launcher window (or press **Import pack**).
4. Pick the pack and press **PLAY**.

The window is drawn by Microsoft Edge (or Chrome) in app mode. If neither is installed, the
launcher opens in a normal browser tab instead.

## What is in it

- Imports CurseForge exports: copies the pack's configs, downloads the mods, installs Minecraft and
  the pack's mod loader (Forge, NeoForge, Fabric or Quilt).
- Mod list per pack with names, versions, pictures, search, and an on/off switch for each mod.
- Memory dropdown per pack, plus a default in Settings. Choices are limited to what the PC has.
- Pack pictures: click the picture to use your own image.
- Missing files tab for mods whose authors only allow downloads from the CurseForge site. Download
  them in your browser, leave them in Downloads, press **Check Downloads**.
- Console tab with the game's output and a copy button, playtime and last-played tracking,
  folder shortcuts, rename, delete, and a New instance dialog for building your own setup.

## Built-in search

- **Find modpacks** (left side): searches Modrinth and CurseForge for whole packs and installs
  one with a click, including its picture. Modrinth packs (`.mrpack`) can also be dropped onto the
  window like CurseForge zips.
- **Add mods** tab (inside a pack): searches Modrinth and CurseForge for mods, resource packs and
  shaders that fit that pack's Minecraft version and mod loader. Install adds the newest release
  and any add-ons it requires. Installing again later updates it.

Modrinth needs no setup. CurseForge search needs the free API key below. Mods whose authors only
allow downloads from the CurseForge site cannot be installed from the launcher; it says so and
links the page.

## CurseForge API key (recommended)

Settings has a field for a free key from https://console.curseforge.com. With it, imports are
faster and checksum-verified, and the mod list shows CurseForge names and pictures. Without it the
launcher uses the website's public download link for each file, which CurseForge may block.

## Signing in: play through the Minecraft Launcher

This is the default when the official Minecraft Launcher is installed, and needs no setup.

Pressing **PLAY** installs the pack's mod loader into the Minecraft Launcher's own folder, adds the
pack there as an installation (pointing at the pack's folder here, with your memory setting), and
opens the Minecraft Launcher. Pick the pack next to its Play button and press Play. You are signed
in with your normal Microsoft account, so every server works.

- PLAY opens the Minecraft Launcher for you. If it is already open, PLAY closes it first and
  reopens it, because it overwrites its installation list when it closes.
- Your Minecraft player name and face are shown in the bottom-left corner. They are read from the
  Minecraft Launcher's account file (public profile only, never the sign-in tokens); the skin
  picture comes from Mojang's servers.
- The Minecraft Launcher must have been opened once on the computer.
- Mods, memory and pack changes are still managed here. Press PLAY here again after changing the
  memory setting.
- The first copy of the Minecraft Launcher's installation list is kept next to it as a
  `.monkeyboyz-backup` file.
- Discord status cannot show the pack in this mode, because the game is not started by this launcher.

"Play directly" (in the pack's menu, or Settings > Play using) starts the game from this launcher
instead, with an offline player or the optional Microsoft setup below.

## Microsoft sign-in inside this launcher (optional)

Press the account box, then **Sign in with Microsoft**. The launcher shows a code, opens Microsoft's
page in your browser, and finishes by itself once you approve.

Microsoft requires each launcher to have its own app ID that Mojang has approved, so this needs a
one-time setup by one person (the launcher walks through it):

1. Register a free app at portal.azure.com (personal Microsoft accounts only, "Allow public client
   flows" on).
2. Ask Mojang to approve it using the form linked from
   https://help.minecraft.net/hc/en-us/articles/16254801392141
3. Share the Application (client) ID with the group. Each person either pastes it once in
   Settings, or you put it in a text file named `ms_client_id.txt` next to the exe and share both.

Until then, offline players work for single player and offline-mode servers.

Sign-in tokens are stored in `config.json` in the data folder, unencrypted.

## Where things live

`%APPDATA%\MonkeyBoyzLauncher` (Settings shows the folder and opens it). Each pack has its own
`minecraft` folder with `mods`, `config` and `saves`. Game files and Java are shared between packs.

## Building from source

Needs Go 1.21 or newer, nothing else.

```
go test ./...
go build -ldflags "-H windowsgui -s -w" -o MonkeyBoyzLauncher.exe .
```

To bake a Microsoft client ID into the exe, add `-X main.builtinMSClientID=<id>` inside `-ldflags`.

## Testing status

`go test` runs the whole pipeline against a simulated internet with a stand-in game: import,
both mod download paths, a Forge-style installer step, the mod list, Microsoft sign-in, and a real
process launch. The interface was checked in Chrome on Linux.

It has not yet been run on Windows, with the real Minecraft Launcher, or against the real Mojang,
Forge, NeoForge, Fabric, Quilt, CurseForge and Microsoft servers. If something fails, the Console tab shows why.

Not supported: Forge for Minecraft 1.5 and older, Modrinth packs, updating a pack in place.

## Discord status

With Discord open, the launcher shows "Playing <pack name>" with the Minecraft version, mod loader,
mod count and elapsed time. Discord needs an application ID for this (free, one per group):

1. Go to https://discord.com/developers/applications, press New Application and name it (the name
   is what Discord shows after "Playing", for example "MonkeyBoyz Launcher").
2. Optional: under Rich Presence > Art Assets, upload a picture named `logo`.
3. Copy the Application ID into Settings, or put it in `discord_app_id.txt` next to the exe/Setup.

Turn it off under Settings > Discord status.

## Setup program

`setup/` builds the installer that ships in the zip:

```
go build -ldflags "-H windowsgui -s -w" -o setup/MonkeyBoyzLauncher.exe .
go build -ldflags "-H windowsgui -s -w" -o "MonkeyBoyz Launcher Setup.exe" ./setup
```

It installs to `%LOCALAPPDATA%\Programs\MonkeyBoyzLauncher` (no administrator rights), adds Desktop
and Start Menu shortcuts and an uninstaller. `ms_client_id.txt` and `discord_app_id.txt` placed next
to Setup are copied along, so friends do not have to type any IDs.
