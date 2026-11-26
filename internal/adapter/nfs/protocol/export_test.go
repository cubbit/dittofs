package protocol

// pauseAfterWindow makes every check on t's contexts call f once the window
// has taken its number and before the table's lists move, so a test holds a
// call there while another runs.
func (t *gssTable) pauseAfterWindow(f func(*gssContext)) { t.afterWindow = f }
