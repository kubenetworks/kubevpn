#!/bin/sh

set -e -x

golangci-lint run . examples/*

go test

if [ "$(go env GOARCH)" = amd64 ]; then
	go test -tags nounsafe
	GOARCH=386 go test
fi

for e in examples/*; do
	(cd $e && go build && rm $(basename $e))
done
