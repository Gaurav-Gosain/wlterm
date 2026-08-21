#!/bin/bash
cd "$(dirname "$0")"
rm -f wev.log
{ sleep 2; printf "\033[<35;40;20M"; printf "\033[<0;40;20M"; printf "\033[<0;40;20m"; printf "\033[97;;1u"; printf "\033[97;;3u"; sleep 2; } | timeout 6 ./wlterm -pixels 800x600 -mode b64 -log wev.log -- stdbuf -oL wev > /dev/null
grep -E "pointer|keyboard|button|motion|key" wev.log | head -25
