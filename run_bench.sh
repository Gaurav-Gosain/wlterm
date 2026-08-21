#!/bin/bash
cd "$(dirname "$0")"
for cfg in "delta full" "delta rect" "shm full" "b64 full"; do
  set -- $cfg
  rm -f bench.log
  timeout 13 ./wlterm -pixels 1280x720 -mode $1 -fps 1000 -log bench.log -- ./wlbench/wlbench -w 1280 -h 720 -work $2 -dur 8 > /dev/null
  echo "== $1 $2"
  grep BENCH bench.log
  grep stats: bench.log | tail -2
done
