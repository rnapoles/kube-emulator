package controllers

import (
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

type (
	U           = store.U
	ResourceKey = store.ResourceKey
)

const (
	emulatorNode       = objects.EmulatorNode
	emulatorNodeIP     = objects.EmulatorNodeIP
	revisionAnnotation = "deployment.kubernetes.io/revision"
	managedLabel       = "emulator.kube/managed"
)

var (
	fromU     = objects.FromU
	toU       = objects.ToU
	initMeta  = objects.InitMeta
	normalize = objects.Normalize
	gs        = objects.GS
	gi        = objects.GI
)

var (
	kNamespaces  = registry.KNamespaces
	kNodes       = registry.KNodes
	kPods        = registry.KPods
	kServices    = registry.KServices
	kEndpoints   = registry.KEndpoints
	kSAs         = registry.KSAs
	kDeployments = registry.KDeployments
	kReplicaSets = registry.KReplicaSets
	kStatefulSet = registry.KStatefulSets
	kDaemonSets  = registry.KDaemonSets
	kJobs        = registry.KJobs
	kReplicationControllers = registry.KReplicationControllers
)
