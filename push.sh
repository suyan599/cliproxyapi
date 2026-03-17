#!/bin/bash

# 颜色定义
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}========================================${NC}"
echo -e "${YELLOW}📦 推送代码到 GitHub${NC}"
echo -e "${BLUE}========================================${NC}"

# 显示并校准远程仓库
EXPECTED_REMOTE="https://github.com/suyan599/cliproxyapi"
REMOTE_URL=$(git remote get-url origin 2>/dev/null)
if [ -z "$REMOTE_URL" ]; then
    echo -e "${YELLOW}⚠️  未配置远程仓库，正在设置为 ${EXPECTED_REMOTE}${NC}"
    git remote add origin "$EXPECTED_REMOTE"
    REMOTE_URL="$EXPECTED_REMOTE"
elif [ "$REMOTE_URL" != "$EXPECTED_REMOTE" ]; then
    echo -e "${YELLOW}⚠️  远程仓库不匹配，正在更新为 ${EXPECTED_REMOTE}${NC}"
    git remote set-url origin "$EXPECTED_REMOTE"
    REMOTE_URL="$EXPECTED_REMOTE"
fi
echo -e "${BLUE}📍 远程仓库: ${REMOTE_URL}${NC}"
echo ""

# 添加所有更改
echo -e "${GREEN}1️⃣  添加所有更改...${NC}"
git add .

HAS_CHANGES=1

# 检查是否有更改
if git diff --cached --quiet; then
    echo -e "${YELLOW}⚠️  没有需要提交的更改${NC}"
    HAS_CHANGES=0
fi

# 获取提交信息
if [ $HAS_CHANGES -eq 1 ]; then
    if [ -z "$1" ]; then
        COMMIT_MSG="Update: $(date '+%Y-%m-%d %H:%M:%S')"
    else
        COMMIT_MSG="$1"
    fi

    # 提交
    echo -e "${GREEN}2️⃣  提交更改: ${COMMIT_MSG}${NC}"
    git commit -m "$COMMIT_MSG"
else
    echo -e "${BLUE}2️⃣  跳过提交，继续推送当前分支与 tag${NC}"
fi

# 推送到 GitHub
echo -e "${GREEN}3️⃣  推送到 ${REMOTE_URL}...${NC}"
echo -e "${YELLOW}💡 提示：如果需要输入用户名和密码，请使用 GitHub Personal Access Token${NC}"

# 先尝试拉取远程更改
git pull origin main --rebase 2>/dev/null
if [ $? -ne 0 ]; then
    echo -e "${YELLOW}⚠️  检测到远程仓库有新内容，正在合并...${NC}"
    git pull origin main --rebase --allow-unrelated-histories
fi

# 推送
git push origin main

if [ $? -ne 0 ]; then
    echo ""
    echo -e "${RED}❌ 推送失败${NC}"
    echo -e "${YELLOW}💡 可能的原因：${NC}"
    echo -e "   1. 需要配置 GitHub 认证（Personal Access Token）"
    echo -e "   2. 网络连接问题"
    echo -e "   3. 没有推送权限"
    exit 1
fi

echo ""
echo -e "${GREEN}✅ 代码推送成功！${NC}"
echo -e "${BLUE}🔗 查看仓库: ${REMOTE_URL}${NC}"

# 自动打 tag 触发 GitHub Actions 构建 Docker 镜像
echo ""
echo -e "${GREEN}4️⃣  创建并推送 tag 以触发镜像构建...${NC}"
TAG="v$(date '+%Y%m%d%H%M%S')"
git tag "$TAG"
git push origin "$TAG"

if [ $? -eq 0 ]; then
    echo ""
    echo -e "${GREEN}✅ Tag ${TAG} 推送成功，GitHub Actions 将自动构建镜像${NC}"
    echo -e "${BLUE}🔗 查看构建进度: ${REMOTE_URL}/actions${NC}"
else
    echo ""
    echo -e "${RED}❌ Tag 推送失败${NC}"
    exit 1
fi
