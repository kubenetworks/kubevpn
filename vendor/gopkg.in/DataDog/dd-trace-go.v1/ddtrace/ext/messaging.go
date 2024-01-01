// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package ext

const (
	// MessagingSystem identifies which messaging system created this span (kafka, rabbitmq, amazonsqs, googlepubsub...)
	MessagingSystem = "messaging.system"
)

// Available values for messaging.system.
const (
	MessagingSystemGCPPubsub = "googlepubsub"
	MessagingSystemKafka     = "kafka"
)

// Kafka tags.
const (
	// MessagingKafkaPartition defines the Kafka partition the trace is associated with.
	MessagingKafkaPartition = "messaging.kafka.partition"
	// KafkaBootstrapServers holds a comma separated list of bootstrap servers as defined in producer or consumer config.
	KafkaBootstrapServers = "messaging.kafka.bootstrap.servers"
)
