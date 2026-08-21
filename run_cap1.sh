#!/bin/bash
cd "$(dirname "$0")"
rm -f cap1.out cap1.log cap1.stamps
export LIBGL_ALWAYS_SOFTWARE=1
{ sleep 6; printf "echo kitty inside a tuios pane\r"; sleep 4; } | timeout 15 ./wlterm -pixels 1152x720 -mode b64 -stamp cap1.stamps -log cap1.log -- kitty -o confirm_os_window_close=0 -e env XDG_RUNTIME_DIR=/run/user/1000/wlterm-rt-a /home/gaurav/go/bin/tuios > cap1.out
echo exit=$?; wc -c cap1.out; wc -l cap1.stamps
