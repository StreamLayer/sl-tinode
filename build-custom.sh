#!/usr/bin/env bash

set -ex

### builds custom image for makeomatic purposes
# usage "./build-custom.sh tag=v0.15.9"
goplat=( $TARGETOS )
goarc=( $TARGETARCH )
dbtags=( mongodb )

export GOPATH=`go env GOPATH`

for line in $@; do
  eval "$line"
done

version=${tag#?}
push=${push:-true}
releasepath=${releasepath:-"./docker/tinode/releases"}
repository=${repository:-"gcr.io/peak-orbit-214114"}

if [ -z "$version" ]; then
    echo "Must provide tag as 'tag=v1.2.3'"
    exit 1
fi

echo "Releasing $version"

GOSRC=${GOPATH}/src/github.com/tinode
git submodule update --init --recursive

# Prepare directory for the new release
rm -fR ${releasepath}/${version}
mkdir -p ${releasepath}/${version}


for plat in "${goplat[@]}"
do
  for arc in "${goarc[@]}"
  do
    # Keygen is database-independent
    # Remove previous build
    rm -f $GOPATH/bin/keygen

    # Build
    env GOOS=${plat} GOARCH=${arc} go build -o $GOPATH/bin/keygen \
      -ldflags "-s -w -extldflags \"-fno-PIC -static\"" \
      -tags 'osusergo netgo static_build' \
      ./keygen > /dev/null

    for dbtag in "${dbtags[@]}"
    do
      echo "Building ${dbtag}-${plat}/${arc}..."
      tmppath=`mktemp -d`

      # Remove previous builds
      rm -f $GOPATH/bin/tinode
      rm -f $GOPATH/bin/init-db

      # Build tinode server and database initializer for RethinkDb and MySQL.
      env GOOS=${plat} GOARCH=${arc} CGO_ENABLED=1 go build -o $GOPATH/bin/tinode -race \
        -ldflags "-s -w -extldflags \"-fno-PIC -static\" -X main.buildstamp=${version}" \
        -tags "${dbtag} osusergo netgo static_build" \
        ./server > /dev/null

      env GOOS=${plat} GOARCH=${arc} go build  \
        -ldflags "-s -w -extldflags \"-fno-PIC -static\"" \
        -tags "${dbtag} osusergo netgo static_build" \
        -o $GOPATH/bin/init-db ./tinode-db > /dev/null

      # Tar on Mac is inflexible about directories. Let's just copy release files to
      # one directory.
      mkdir -p ${tmppath}/static/img
      mkdir ${tmppath}/static/css
      mkdir ${tmppath}/static/audio
      mkdir ${tmppath}/static/src
      mkdir ${tmppath}/static/umd
      mkdir ${tmppath}/templ

      # Copy templates and database initialization files
      cp ./server/tinode.conf ${tmppath}
      cp ./server/templ/*.templ ${tmppath}/templ
      cp ./server/static/img/*.png ${tmppath}/static/img
      cp ./server/static/img/*.svg ${tmppath}/static/img
      cp ./server/static/audio/*.m4a ${tmppath}/static/audio
      cp ./server/static/css/*.css ${tmppath}/static/css
      cp ./server/static/index.html ${tmppath}/static
      cp ./server/static/index-dev.html ${tmppath}/static
      cp ./server/static/version.js ${tmppath}/static
      cp ./server/static/umd/*.js ${tmppath}/static/umd
      cp ./server/static/manifest.json ${tmppath}/static
      cp ./server/static/service-worker.js ${tmppath}/static
      # Create empty FCM client-side config.
      echo > ${tmppath}/static/firebase-init.js
      cp ./tinode-db/data.json ${tmppath}
      cp ./tinode-db/*.jpg ${tmppath}
      cp ./tinode-db/credentials.sh ${tmppath}

      # Build archive. All platforms but Windows use tar for archiving. Windows uses zip.
      # Copy binaries
      cp $GOPATH/bin/tinode ${tmppath}
      cp $GOPATH/bin/init-db ${tmppath}
      cp $GOPATH/bin/keygen ${tmppath}

      # Remove possibly existing archive.
      rm -f ${releasepath}/${version}/tinode-${dbtag}."${plat}-${arc}".tar.gz
      # Generate a new one
      tar -C ${tmppath} -zcf ${releasepath}/${version}/tinode-${dbtag}."${plat}-${arc}".tar.gz .

      rm -fR ${tmppath}
    done
  done
done

# for dbtag in "${dbtags[@]}"
# do
#   rmitags="${repository}/tinode-${dbtag}:${version}"

#   if [ x"$push" = x"true" ]; then
#     docker rmi ${rmitags} -f
#   fi

#   docker build --build-arg VERSION=$version --build-arg TARGET_DB=${dbtag} --tag ${rmitags} docker/tinode

#   if [ x"$push" = x"true" ]; then
#     docker push $rmitags
#   fi
# done
