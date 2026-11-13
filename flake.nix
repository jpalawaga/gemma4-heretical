{
  description = "Local Gemma 4 chat and Kokoro speech tools for Apple Silicon";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        python = pkgs.python312;
      in {
        devShells.default = pkgs.mkShell {
          buildInputs = [
            python
            python.pkgs.pip
            python.pkgs.virtualenv
            pkgs.git
            pkgs.cacert
            pkgs.go
            pkgs.uv
          ] ++ pkgs.lib.optionals pkgs.stdenv.isDarwin [
            pkgs.imagesnap
          ];

          shellHook = ''
            export PROJECT_ROOT="$(pwd)"
            export VENV_DIR="$PROJECT_ROOT/.venv"

            if [ ! -d "$VENV_DIR" ]; then
              echo "Creating Python 3.12 virtualenv..."
              virtualenv "$VENV_DIR"
            fi

            source "$VENV_DIR/bin/activate"

            if ! python -c "import heretic" 2>/dev/null; then
              echo "Installing heretic-llm (this may take a while on first run)..."
              pip install -U heretic-llm 2>&1 | tail -5
            fi

            echo ""
            echo "  gemma4-heretical dev shell (legacy DIY abliteration tools)"
            echo "  Python: $(python --version)"
            echo "  heretic: $(pip show heretic-llm 2>/dev/null | grep Version || echo 'installing...')"
            echo ""
            echo "  ./get-gemma4-heretical — install the 26B-A4B model in Ollama"
            echo "  ./get-gemma4-llama — download the vision + MTP GGUF files"
            echo "  ./serve-llama — serve vision + MTP through llama.cpp"
            echo "  go build -o chat2 ./cmd/chat2 — build the Charm TUI"
            echo "  go build -o chat3 ./cmd/chat3 — build the basic chat TUI"
            echo "  ./tts — run local Kokoro text-to-speech"
            echo "  ./abliterate    — run legacy heretic flow on gemma-4-31B-it"
            echo "  ./convert-to-ollama — convert result to GGUF + import to Ollama"
            echo ""
          '';
        };
      });
}
