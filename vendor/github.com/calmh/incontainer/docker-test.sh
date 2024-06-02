#!/bin/bash

docker run -v $(pwd):/src -w /src -e IN_DOCKER_TEST=1 golang:latest go test -v
