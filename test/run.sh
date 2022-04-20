#!/bin/bash

kubectl apply -f pod.yaml
kubectl wait --for=condition=Ready pod/traffic-test
cd ./server && GOARCH=amd64 GOOS=linux go build -o main
kubectl cp main traffic-test:/app/main
rm -fr main
kubectl port-forward pods/traffic-test 1080
