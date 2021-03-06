#!/bin/bash

# This script downloads and installs the grpc library, for
# go and python, in the root of the image. It assumes we're running
# as root in the image.
set -ex

# grpc_dist can be empty, in which case we just install to the default paths
grpc_dist="$1"
if [ "$grpc_dist" != "" ]; then
  cd $grpc_dist
fi

git clone https://github.com/grpc/grpc.git
cd grpc
git checkout 82c8f71a81b707376a72257b294fe6b6f1f5219d # Beta Release 0.11.0
git submodule update --init
make
if [ "$grpc_dist" != "" ]; then
  make install prefix=$grpc_dist
else
  make install
fi
CONFIG=opt ./tools/run_tests/build_python.sh
if [ "$grpc_dist" != "" ]; then
  CFLAGS=-I$grpc_dist/include LDFLAGS=-L$grpc_dist/lib pip install src/python/grpcio -t $grpc_dist/lib/python2.7/site-packages
else
  pip install src/python/grpcio
fi
