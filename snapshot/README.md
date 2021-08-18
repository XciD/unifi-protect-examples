# Snapshot

On this example, we implement an HTTP Server that return a snapshot of any camera.

We could use the `http://ip/snap.jpeg`, but, the latency is quite high (~300ms) for one picture.

On this example, we connect to the livefeed, pipe the fmp4 stream to ffmpeg and convert to an image

Final latency: ~4ms

# Requirement

- ffmpeg

