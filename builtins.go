package anymd

// registry collects the built-in converters. Each converter file registers
// itself from an init(), so adding a format touches exactly one new file and
// never this one. Go runs a package's init()s in filename order, which makes
// registration deterministic; ordering that matters is expressed through
// Priority, not through filenames.
var registry []Converter

// addBuiltin is called from each converter file's init().
func addBuiltin(c Converter) { registry = append(registry, c) }

func builtins() []Converter { return registry }
