package handler

// ControlSession is a named type alias for ConnectOptions.
// It documents that ConnectOptions is the user-daemon (control-plane) session type:
// it manages the traffic manager, proxy injection, and persisted connection state.
//
// All existing code that uses ConnectOptions is unaffected — this alias provides
// semantic clarity for new code, documentation, and the daemon/action layer.
// Both &ControlSession{} and &ConnectOptions{} compile to identical types.
type ControlSession = ConnectOptions
