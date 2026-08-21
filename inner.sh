#!/bin/bash
cd "$(dirname "$0")"
exec env XDG_RUNTIME_DIR=/run/user/1000 ./wlterm -mode b64 -log inner2.log -- env LIBGL_ALWAYS_SOFTWARE=1 kitty -o confirm_os_window_close=0 -e env XDG_RUNTIME_DIR=/run/user/1000/wlterm-rt-b /home/gaurav/go/bin/tuios
