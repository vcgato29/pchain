#!/usr/bin/env bash
set -e

# Get the version from the environment, or try to figure it out.
if [ -z $VERSION ]; then
	VERSION=$(awk -F\" '/Version =/ { print $2; exit }' < version/version.go)
fi
if [ -z "$VERSION" ]; then
    echo "Please specify a version."
    exit 1
fi
echo "==> Building version $VERSION..."

# Get the parent directory of where this script is.
SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ] ; do SOURCE="$(readlink "$SOURCE")"; done
DIR="$( cd -P "$( dirname "$SOURCE" )/.." && pwd )"

# Change into that dir because we expect that.
cd "$DIR"

# Generate the tag.
if [ -z "$NOTAG" ]; then
  echo "==> Tagging..."
  git commit --allow-empty -a -m "Release v$VERSION"
  git tag -a -m "Version $VERSION" "v${VERSION}" master
fi

# Do a hermetic build inside a Docker container.
docker build -t tendermint/tendermint-builder scripts/tendermint-builder/
docker run --rm -e "BUILD_TAGS=$BUILD_TAGS" -v "$(pwd)":/go/src/github.com/tendermint/tendermint tendermint/tendermint-builder ./scripts/dist_build.sh

# Add "tendermint" and $VERSION prefix to package name.
rm -rf ./build/dist
mkdir -p ./build/dist
for FILENAME in $(find ./build/pkg -mindepth 1 -maxdepth 1 -type f); do
  FILENAME=$(basename "$FILENAME")
	cp "./build/pkg/${FILENAME}" "./build/dist/tendermint_${VERSION}_${FILENAME}"
done

# Make the checksums.
pushd ./build/dist
shasum -a256 ./* > "./tendermint_${VERSION}_SHA256SUMS"
popd

# Done
echo
echo "==> Results:"
ls -hl ./build/dist

exit 0
