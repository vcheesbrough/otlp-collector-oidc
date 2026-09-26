// Package render turns the environment into the collector configuration the
// process runs on.
//
// The documented environment variables are the whole configuration
// interface. Each is a tagged field on a group struct (env.go): the tag holds
// its name, default, required-ness and description, once. From those tags
// the environment is parsed and validated (env.go), rendered to YAML through
// the embedded template (render.go), and documented (docs.go). The run
// command (run.go) chooses between the rendered pipeline and a mounted
// COLLECTOR_CONFIG and hands either to the collector through the same file
// provider, so the collector cannot tell them apart.
//
// Rendering only maps: a variable's value reaches the YAML key it names, with
// no knowledge of what the component does with it.
package render
