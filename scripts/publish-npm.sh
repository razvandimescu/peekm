#!/bin/bash
set -e

# This script publishes peek to npm
# Run after goreleaser has built the binaries

VERSION=$1

if [ -z "$VERSION" ]; then
  echo "Usage: $0 <version>"
  echo "Example: $0 0.1.0"
  exit 1
fi

echo "Publishing peek version $VERSION to npm..."

# Update version in all package.json files
find npm -name "package.json" -type f -exec sed -i.bak "s/\"version\": \".*\"/\"version\": \"$VERSION\"/" {} \;
find npm -name "*.bak" -delete

# Copy binaries from dist/ to npm platform directories
echo "Copying binaries..."
cp dist/peek_darwin_arm64/peek npm/platforms/darwin-arm64/bin/
cp dist/peek_darwin_amd64/peek npm/platforms/darwin-x64/bin/
cp dist/peek_linux_arm64/peek npm/platforms/linux-arm64/bin/
cp dist/peek_linux_amd64/peek npm/platforms/linux-x64/bin/
cp dist/peek_windows_amd64/peek.exe npm/platforms/win32-x64/bin/

# Publish platform packages first
echo "Publishing platform packages..."
for platform in darwin-arm64 darwin-x64 linux-arm64 linux-x64 win32-x64; do
  echo "Publishing @peek/$platform..."
  (cd npm/platforms/$platform && npm publish --access public)
done

# Wait a bit for npm to propagate
echo "Waiting for npm to propagate..."
sleep 5

# Publish main package
echo "Publishing main package..."
(cd npm && npm publish --access public)

echo "✓ Successfully published peek@$VERSION to npm"
