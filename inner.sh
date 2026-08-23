#!/bin/bash
# Level two of the recursion: run wlterm inside a tuios pane that is itself
# inside wlterm. tuios gets throwaway XDG dirs so it neither restores nor
# overwrites the real session.
cd "$(dirname "$0")"
PRIV=$(mktemp -d /tmp/wlterm-inner-priv.XXXXXX)
RT=$PRIV/rt; mkdir -p "$RT"; chmod 700 "$RT"
TUIOS=$(command -v tuios || echo "$HOME/.local/bin/tuios")
exec env XDG_RUNTIME_DIR=/run/user/$(id -u) ./wlterm -- \
  env LIBGL_ALWAYS_SOFTWARE=1 kitty -o confirm_os_window_close=0 -e \
  env XDG_CONFIG_HOME="$PRIV/config" XDG_STATE_HOME="$PRIV/state" \
      XDG_DATA_HOME="$PRIV/data" XDG_CACHE_HOME="$PRIV/cache" \
      XDG_RUNTIME_DIR="$RT" "$TUIOS"
