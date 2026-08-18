package repositories

// These package-level markers keep the shared manager ClusterRole in sync with
// the Worktree controller's permissions.
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=repositories.rc.ayaka.io,resources=worktrees/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
