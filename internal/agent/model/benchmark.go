package model

// BenchmarkRequest binds the single temporary-LV run to immutable pool, node,
// and device identities. It never accepts arbitrary fio options or a target LV.
type BenchmarkRequest struct {
	PoolUID  string
	NodeUID  string
	VGName   string
	RunID    string
	Device   Device
	Approved bool
}
