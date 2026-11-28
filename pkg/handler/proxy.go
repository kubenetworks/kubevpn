package handler

// Resources identifies a workload by namespace and name for leave operations.
type Resources struct {
	Namespace string
	Workload  string
}
