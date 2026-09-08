#!/bin/bash
# AnyDev 云开发环境初始化脚本
# 幂等：可重复执行，已安装的组件会跳过
#
# 安装内容：
#   1. Go 1.25（/usr/local/go）— MCP 编译
#   2. Node.js 22 + npm（/usr/local/node-v22.x）— CodeBuddy CLI
#   3. CodeBuddy CLI（npm 全局 + symlink）
#   4. Python 依赖（websockets, pyyaml, httpx, fastapi, uvicorn）— Mock UE + Adapter
#   5. Ollama（反应层本地模型）
#   6. PATH 配置到 /root/.bashrc（漫游保留，环境重建不丢）
#
# 用法：
#   bash scripts/setup-cloud-env.sh
#
# 注意：
#   - 幂等，可重复执行
#   - 环境销毁后 /usr/local/ 丢失，重新跑此脚本即可恢复
#   - /root/.bashrc 漫游保留，PATH 配置不会丢
#   - pip 缓存走 /root/.cache/pip，漫游保留，二次安装快
#   - Ollama 模型放 /usr/share/ollama/.ollama，环境销毁会丢，需重新 pull

set -euo pipefail

# ─── 颜色输出 ──────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }

# ─── 配置 ──────────────────────────────────────────────────────
GO_VERSION="1.25.0"
NODE_VERSION="v22.11.0"
NODE_BASE="node-${NODE_VERSION}-linux-x64"
NODE_TAR="${NODE_BASE}.tar.xz"
NODE_URL="https://nodejs.org/dist/${NODE_VERSION}/${NODE_TAR}"
GO_URL="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
PYTHON_DEPS="websockets pyyaml httpx fastapi uvicorn"
NPM_GLOBAL_PREFIX="/root/.npm/node_modules"
OLLAMA_MODEL="qwen2.5:7b-instruct-q4_K_M"

# ─── 1. Go ─────────────────────────────────────────────────────
install_go() {
    info "=== Step 1: Go ${GO_VERSION} ==="
    if [ -x /usr/local/go/bin/go ]; then
        ok "Go already installed: $(/usr/local/go/bin/go version)"
        return 0
    fi

    info "Downloading Go ${GO_VERSION}..."
    cd /tmp
    curl -fsSL -o go.tar.gz "${GO_URL}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf go.tar.gz
    rm -f go.tar.gz
    ok "Go ${GO_VERSION} installed to /usr/local/go"
}

# ─── 2. Node.js ────────────────────────────────────────────────
install_node() {
    info "=== Step 2: Node.js ${NODE_VERSION} ==="
    if command -v node &>/dev/null && [ "$(node --version)" = "${NODE_VERSION}" ]; then
        ok "Node.js already installed: $(node --version)"
        return 0
    fi

    info "Downloading Node.js ${NODE_VERSION}..."
    cd /tmp
    curl -fsSL -o "${NODE_TAR}" "${NODE_URL}"
    tar -C /usr/local -xJf "${NODE_TAR}"
    rm -f "${NODE_TAR}"

    # symlink 到 /usr/local/bin（非交互式 SSH 可能读不到 .bashrc 里的 PATH）
    ln -sf "/usr/local/${NODE_BASE}/bin/node" /usr/local/bin/node
    ln -sf "/usr/local/${NODE_BASE}/bin/npm" /usr/local/bin/npm
    ln -sf "/usr/local/${NODE_BASE}/bin/npx" /usr/local/bin/npx
    ok "Node.js ${NODE_VERSION} installed"
}

# ─── 3. CodeBuddy CLI ──────────────────────────────────────────
install_codebuddy() {
    info "=== Step 3: CodeBuddy CLI ==="
    if command -v codebuddy &>/dev/null; then
        ok "CodeBuddy CLI already installed: $(codebuddy --version 2>/dev/null | head -1)"
        return 0
    fi

    # npm 全局安装路径用非标准 prefix（/root/.npm/... 在 /root 下，漫游保留）
    npm config set prefix "${NPM_GLOBAL_PREFIX}"

    info "Installing @tencent-ai/codebuddy-code..."
    npm install -g @tencent-ai/codebuddy-code

    # symlink（npm prefix 非标准，需手动 link 到 /usr/local/bin）
    local cb_bin="${NPM_GLOBAL_PREFIX}/lib/node_modules/@tencent-ai/codebuddy-code/bin/codebuddy"
    if [ -f "$cb_bin" ]; then
        ln -sf "$cb_bin" /usr/local/bin/codebuddy
        ok "CodeBuddy CLI installed and symlinked to /usr/local/bin/codebuddy"
    else
        fail "CodeBuddy CLI install failed: binary not found at ${cb_bin}"
    fi
}

# ─── 4. Python 依赖 ────────────────────────────────────────────
install_python_deps() {
    info "=== Step 4: Python dependencies ==="

    if ! command -v python3 &>/dev/null; then
        fail "python3 not found. Install: yum install -y python3 python3-pip"
    fi
    ok "Python: $(python3 --version)"

    # 不加 --upgrade：避免升级已装包破坏其他项目依赖
    # （例如 Hermes 要求 websockets==15.0.1，upgrade 会升到 17.0 导致不兼容）
    info "Installing missing deps: ${PYTHON_DEPS}"
    pip3 install ${PYTHON_DEPS}
    ok "Python deps installed"
}

# ─── 5. Ollama ────────────────────────────────────────────────
install_ollama() {
    info "=== Step 5: Ollama (reactive layer) ==="
    if command -v ollama &>/dev/null; then
        ok "Ollama already installed: $(ollama --version 2>/dev/null || echo 'version unknown')"
    else
        info "Installing Ollama..."
        curl -fsSL https://ollama.com/install.sh | sh
        ok "Ollama installed"
    fi

    # 拉取反应层模型（~4GB，环境销毁后需重新 pull）
    info "Pulling reactive layer model ${OLLAMA_MODEL}..."
    info "This may take a while (~4GB). If it fails, run manually:"
    info "  ollama pull ${OLLAMA_MODEL}"
    ollama pull "${OLLAMA_MODEL}" || warn "Ollama pull failed — run manually when ready"
}

# ─── 6. PATH 配置 ──────────────────────────────────────────────
setup_path() {
    info "=== Step 6: PATH configuration (.bashrc) ==="

    local bashrc="/root/.bashrc"
    local path_line='export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/usr/local/go/bin:/root/go/bin'

    touch "$bashrc"

    # 检查是否已配置 Go PATH（幂等）
    if grep -q "/usr/local/go/bin" "$bashrc"; then
        ok "Go PATH already configured in .bashrc"
    else
        echo '' >> "$bashrc"
        echo '# Cloud dev environment PATH (setup-cloud-env.sh)' >> "$bashrc"
        echo "$path_line" >> "$bashrc"
        ok "Go PATH added to .bashrc"
    fi

    # 确保 ~/.local/bin 在 PATH
    if ! grep -q '.local/bin' "$bashrc"; then
        echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$bashrc"
        ok "Added ~/.local/bin to PATH"
    fi
}

# ─── 主流程 ────────────────────────────────────────────────────
info "AnyDev Cloud Environment Setup"
info "================================"
echo ""

install_go
echo ""
install_node
echo ""
install_codebuddy
echo ""
install_python_deps
echo ""
install_ollama
echo ""
setup_path
echo ""

ok "=== Setup Complete ==="
echo ""
echo "Next steps:"
echo "  1. Reload PATH:  source ~/.bashrc"
echo "  2. Build MCP:    cd /data/workspace/dev/agenttown-mcp && go build -o ../mcp ./cmd/agenttown-mcp"
echo "  3. Start MCP:    cd /data/workspace/dev && ./mcp --llm-backend=venus --http :8770 --ws :9091 --venus-api-key \"\$VENUS_API_KEY\" --log-level debug"
echo "  4. Start UE: python3 src/run_day.py"
echo ""
warn "注意：环境销毁后 /usr/local/ 下的安装会丢失，重新跑此脚本即可恢复"
warn "      /root/.bashrc 漫游保留，PATH 配置不会丢"
warn "      Ollama 模型在 /usr/share/ollama，环境销毁需重新 pull"
