# Release Process

This document describes how to create a new release of peekm.

## Prerequisites

1. **GitHub repository**: Ensure you have push access to the repository
2. **Homebrew tap repository**: Create `homebrew-tap` repository at https://github.com/razvandimescu/homebrew-tap
3. **npm account**: Create account at https://www.npmjs.com and generate access token
4. **GitHub secrets**: Add the following secrets to your repository:
   - `HOMEBREW_TAP_GITHUB_TOKEN`: Personal access token with repo write access
   - `NPM_TOKEN`: npm access token for publishing

## Release Steps

### 1. Prepare the Release

Ensure all changes are committed and pushed to main branch:

```bash
git status
git log --oneline -5
```

### 2. Create a Git Tag

Create and push a version tag:

```bash
# For a new version (e.g., 0.1.0)
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

### 3. Automated Release Process

Once the tag is pushed, GitHub Actions will automatically:

1. **Build binaries** for all platforms (via goreleaser)
2. **Create GitHub Release** with:
   - Pre-compiled binaries (`.tar.gz` and `.zip`)
   - Checksums (`checksums.txt`)
   - Auto-generated changelog
3. **Update Homebrew tap** (pushes formula to `homebrew-tap` repo)

### 4. Publish to npm (Manual)

After the GitHub release is complete, publish to npm:

**Option A: Using GitHub Actions** (Recommended)

1. Go to Actions > "Publish to npm"
2. Click "Run workflow"
3. Enter the version number (e.g., `0.1.0`)
4. Click "Run workflow"

**Option B: Manually**

```bash
# Download the release
VERSION=0.1.0
gh release download "v${VERSION}" -D dist

# Run publish script
./scripts/publish-npm.sh $VERSION
```

### 5. Verify the Release

**GitHub Releases**:
```bash
# Visit: https://github.com/razvandimescu/peekm/releases
# Verify binaries are present and checksums are correct
```

**Homebrew**:
```bash
brew tap razvandimescu/tap
brew install peekm
peekm --version
```

**npm**:
```bash
npm install -g peekm
peekm --version
```

## Version Numbering

We follow [Semantic Versioning](https://semver.org/):

- **MAJOR** version (v1.0.0 > v2.0.0): Breaking changes
- **MINOR** version (v0.1.0 > v0.2.0): New features, backwards compatible
- **PATCH** version (v0.1.0 > v0.1.1): Bug fixes, backwards compatible

## Rollback

If a release has issues:

1. **Delete the tag**:
   ```bash
   git tag -d v0.1.0
   git push origin :refs/tags/v0.1.0
   ```

2. **Delete the GitHub release** (via web UI)

3. **Unpublish from npm** (within 72 hours):
   ```bash
   npm unpublish peekm@0.1.0
   # Also unpublish platform packages
   npm unpublish @peekm/darwin-arm64@0.1.0
   # ... etc
   ```

4. **Homebrew** will auto-update on next release

## Post-Release

After a successful release:

1. Update README if needed (remove "Coming soon" labels)
2. Announce on relevant channels
3. Monitor GitHub issues for bug reports

## Testing Pre-releases

To test the release process without publishing:

```bash
# Create an alpha/beta tag
git tag -a v0.1.0-alpha.1 -m "Alpha release"
git push origin v0.1.0-alpha.1

# goreleaser will mark it as pre-release automatically
```

## Troubleshooting

**goreleaser fails**:
- Check `.goreleaser.yml` syntax
- Ensure Go tests pass: `go test ./...`
- Check goreleaser locally: `goreleaser release --snapshot --clean`

**Homebrew tap fails**:
- Verify `HOMEBREW_TAP_GITHUB_TOKEN` secret is set
- Ensure `homebrew-tap` repository exists
- Check token has write permissions

**npm publish fails**:
- Verify `NPM_TOKEN` secret is set
- Ensure package name is available (first release only)
- Check npm package naming: `@peekm/*` scope
