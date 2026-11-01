# LocalAI with its llama.cpp engine built in. LocalAI's image carries no
# inference engines and installs them from its gallery on first use, whose
# llama.cpp entry follows the latest release; this copies the engine image
# of the same release into the system backends folder, which LocalAI scans
# at start, so the engine is pinned and nothing is downloaded at run time.
FROM quay.io/go-skynet/local-ai-backends:v4.11.0-cpu-llama-cpp AS llama-cpp

FROM localai/localai:v4.11.0
COPY --from=llama-cpp / /var/lib/local-ai/backends/llama-cpp/
COPY start.sh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
