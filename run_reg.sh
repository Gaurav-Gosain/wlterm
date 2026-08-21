#!/bin/bash
cd "$(dirname "$0")"
rm -f foot.log foot.out
{ sleep 3; printf "echo hello-again\r"; sleep 2; } | timeout 7 ./wlterm -pixels 800x600 -mode b64 -log foot.log -- foot > foot.out
./kittydec/kittydec -in foot.out -out foot.png
rm -f evil.log
timeout 5 ./wlterm -pixels 400x300 -mode b64 -log evil.log -- ./wlbench/wlbench -work evil > /dev/null
grep EVIL evil.log
grep -c "refusing pool" evil.log
