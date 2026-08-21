#!/bin/bash
cd "$(dirname "$0")"
rm -f cap2.out cap2.log cap2.stamps inner2.log
export LIBGL_ALWAYS_SOFTWARE=1
{ sleep 7; printf "/tmp/claude-1000/-home-gaurav-dev-tuios/31fac879-b893-45de-bf68-908789b7bd39/scratchpad/wlterm/inner.sh\r"; sleep 16; printf "echo turtles all the way down\r"; sleep 6; } | timeout 32 ./wlterm -pixels 1280x800 -mode b64 -stamp cap2.stamps -log cap2.log -- kitty -o confirm_os_window_close=0 -e env XDG_RUNTIME_DIR=/run/user/1000/wlterm-rt-a /home/gaurav/go/bin/tuios > cap2.out
echo exit=$?; wc -c cap2.out; wc -l cap2.stamps; tail -3 inner2.log 2>/dev/null
