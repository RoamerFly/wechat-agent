package companion

// RecordUnavailable is a scope-neutral error for an inaccessible memory.
type RecordUnavailable struct{}

func (RecordUnavailable) Error() string { return "memory_record_unavailable" }

func (RecordUnavailable) SafeToolFailure() (string, string) {
	return "memory_record_unavailable", "当前授权范围内找不到这条记录，或记录已隔离、遗忘；请用memory_search返回的id读取"
}
