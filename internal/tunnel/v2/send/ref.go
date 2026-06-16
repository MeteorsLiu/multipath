package send

import "github.com/MeteorsLiu/multipath/internal/transport"

// Ref is the spec 5.2 design name for a concrete transport reference. During
// migration it aliases transport.LegRef; the zero value Ref{} means "no
// specific transport was requested". Being a type alias, Ref and
// transport.LegRef are the same type and are interchangeable across packages.
type Ref = transport.LegRef
