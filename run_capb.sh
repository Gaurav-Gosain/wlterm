#!/bin/bash
cd "$(dirname "$0")"
rm -f capb.out capb.log innerb.log
export LIBGL_ALWAYS_SOFTWARE=1
{ sleep 7; printf "/tmp/claude-1000/-home-gaurav-dev-tuios/31fac879-b893-45de-bf68-908789b7bd39/scratchpad/wlterm/innerb.sh\r"; sleep 16; } | timeout 27 ./wlterm -pixels 1280x800 -mode b64 -log capb.log -- kitty -o confirm_os_window_close=0 -e env XDG_RUNTIME_DIR=/run/user/1000/wlterm-rt-a /home/gaurav/go/bin/tuios > capb.out
grep BENCH innerb.log
grep stats: innerb.log | tail -3
