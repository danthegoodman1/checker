# Checker

Durable execution via memory checkpointing with support for multiple runtimes

## Why memory checkpointing

Other forms of durable execution solve different problems.

State machines are good for well-defined states, but have a lot of semantic and serialization overhead.

Workflows are good for sequential logic, but fall short when the history or parameters of a step get large, or when state cannot be efficiently (de)serialized.

The next option is memory checkpointing: intentionally pausing the code and making a copy of the memory such that it can be resumed.

Checker allows the code to define _checkpoints_, places where execution is suspended and the state of the program is saved.

This allows you to write the "quick and dirty script" to keep your code short and understandable, but insert checkpoints at critical points in the code to make it durable.

### Example

Stream-parsing massive XML or JSON files.

Because these files are heirarchical in nature, parsers don't have a luxury of resuming at some file offset if there's any nesting.

With Checker, you can structure your code to fetch a large S3 file as sequential range reads instead of a single stream. Fetch a range, parse it, call `checkpoint()`, then fetch the next range. From the parser's perspective, it still sees a continuous byte stream so your parsing code stays simple. But because you've split the download at natural boundaries, checkpoints can happen between ranges without interrupting an active download.

If your process crashes at 9.9GB of a 10GB file, it resumes from the last checkpoint - no need to re-download the file or fast-forward the parser to where you left off.

## Features

- Memory checkpoints defined in the code
- RWMutex for
