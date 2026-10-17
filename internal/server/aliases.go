package server

import (
	"github.com/rnapoles/kube-emulator/internal/objects"
	"github.com/rnapoles/kube-emulator/internal/registry"
	"github.com/rnapoles/kube-emulator/internal/store"
)

type (
	U           = store.U
	ResourceKey = store.ResourceKey
	Store       = store.Store
	Event       = store.Event
	Resource    = registry.Resource
	Registry    = registry.Registry
)

var (
	fromU         = objects.FromU
	initMeta      = objects.InitMeta
	gs            = objects.GS
	gi            = objects.GI
	normalize     = objects.Normalize
	validateTyped = objects.ValidateTyped
	validateMeta  = objects.ValidateMeta
)

var (
	kNamespaces  = registry.KNamespaces
	kNodes       = registry.KNodes
	kPods        = registry.KPods
	kServices    = registry.KServices
	kEndpoints   = registry.KEndpoints
	kSAs         = registry.KSAs
	kPVCs        = registry.KPVCs
	kDeployments = registry.KDeployments
	kReplicaSets = registry.KReplicaSets
	kStatefulSet = registry.KStatefulSets
	kDaemonSets  = registry.KDaemonSets
	kJobs        = registry.KJobs
	kCRDs        = registry.KCRDs
)
