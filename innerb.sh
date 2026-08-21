#!/bin/bash
cd "$(dirname "$0")"
exec env XDG_RUNTIME_DIR=/run/user/1000 ./wlterm -mode shm -fps 1000 -log innerb.log -- ./wlbench/wlbench -w 960 -h 640 -work full -dur 8
