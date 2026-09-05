package imports

// BySession indexes every session in recs by session id. The map aliases
// recs (pointers into the slice and its Sessions), so mutate through the
// refs or not at all. When the same id appears in more than one record the
// later entry in recs wins — with Load's ordering that is the most recent
// import. Sessions with an empty id are ignored.
func BySession(recs []Record) map[string]SessionRef {
	idx := make(map[string]SessionRef)
	for i := range recs {
		rec := &recs[i]
		for j := range rec.Sessions {
			s := &rec.Sessions[j]
			if s.ID == "" {
				continue
			}
			idx[s.ID] = SessionRef{Record: rec, Session: s}
		}
	}
	return idx
}
