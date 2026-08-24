#!/bin/bash
# Re-check the README's "Running applications" list.
#
# Each application runs headless in its own throwaway HOME, gets a pointer
# move, a click and six keystrokes, and is judged on what reached the
# framebuffer. A browser here must never see your real profile: that is what
# the throwaway HOME and -user-data-dir are for.
cd "$(dirname "$0")"
WT=$PWD/wlterm
OUT=${OUT:-/tmp/wlapps}
rm -rf "$OUT"; mkdir -p "$OUT"

try(){
  local n=$1 dur=$2; shift 2
  local d="$OUT/$n"
  mkdir -p "$d/home/.config" "$d/home/.cache" "$d/home/.local/share" "$d/snaps"
  ( sleep $((dur-12))
    printf '\033[<35;301;261M'; sleep 0.3
    printf '\033[<0;301;261M'; sleep 0.15; printf '\033[<0;301;261m'; sleep 0.5
    for c in 119 108 116 101 114 109; do
      printf "\033[$c;1:1u"; sleep 0.04; printf "\033[$c;1:3u"; sleep 0.04
    done
    sleep 9 ) | (
    unset WAYLAND_DISPLAY DISPLAY DBUS_SESSION_BUS_ADDRESS XAUTHORITY
    unset HYPRLAND_INSTANCE_SIGNATURE SWAYSOCK NIRI_SOCKET
    export HOME="$d/home" XDG_CONFIG_HOME="$d/home/.config" \
           XDG_CACHE_HOME="$d/home/.cache" XDG_DATA_HOME="$d/home/.local/share"
    exec timeout -s TERM "$dur" "$WT" -pixels 1280x760 -cell 9x18 -mode shm -fps 30 \
      -log "$d/log" -snapshots "$d/snaps" -snapshot-every 3s -- "$@" >/dev/null 2>&1 )
  printf "%-10s windows=%-2s errors=%-2s frames=%-3s %s\n" "$n" \
    "$(grep -c 'window added' "$d/log")" \
    "$(grep -cE 'refusing|fault copying|protocol error' "$d/log")" \
    "$(ls "$d/snaps" | wc -l)" \
    "$(grep -oP 'launch (UN)?VERIFIED' "$d/log" | tail -1)"
}

try foot     26 foot -T wlterm sh -c 'echo hello; exec sh'
try chromium 40 chromium --user-data-dir="$OUT/chromium/home/cud" \
                --password-store=basic --no-first-run about:blank
try helium   40 helium-browser --user-data-dir="$OUT/helium/home/cud" \
                --password-store=basic --no-first-run about:blank
try code     40 code --user-data-dir="$OUT/code/home/vsc" \
                --extensions-dir="$OUT/code/home/vsx" --new-window /usr/share/doc
try thunar   30 thunar /usr/share/icons
try dolphin  30 dolphin /usr/share/icons
try vkcube   26 vkcube
try hyprland 30 Hyprland -c "$PWD/nested.conf"

# The same browser without --password-store=basic: gnome-keyring asks for a
# password, which arrives as a second toplevel. It has to float over the
# browser, not replace it.
try dialog 40 chromium --user-data-dir="$OUT/dialog/home/cud" \
                --no-first-run --no-default-browser-check about:blank
echo "== a dialog floats over the application, it does not replace it =="
if grep -q 'window added: 2' "$OUT/dialog/log"; then
  echo "second toplevel appeared; check $OUT/dialog/snaps for the browser behind it"
else
  echo "(no dialog this run: nothing asked for a keyring password)"
fi
echo "== leftover shm =="; ls /dev/shm | grep wlterm || echo "(none)"
