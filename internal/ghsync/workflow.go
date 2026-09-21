package ghsync

// DefaultWorkflowFile is the file name the panel suggests for the workflow it
// dispatches. Any name works; this is the one the setup instructions and the
// generated file agree on, so a copy-paste produces a working pair.
const DefaultWorkflowFile = "mirrorpilot-relay.yml"

// DefaultRef is the branch the workflow is dispatched from when none is given.
//
// A guess, and labelled as one in the UI: the repository's real default branch
// is not something this panel can see, and "main" is right far more often than
// it is wrong.
const DefaultRef = "main"

// WorkflowYAML is the workflow the panel asks you to commit.
//
// It is the whole of what makes the copy work, and it is deliberately small:
// GitHub's runners can reach registries that the machine running this panel
// cannot, so the job is a pull, a tag and a push. Everything with write access
// to a registry is a repository secret; everything else travels as a
// workflow_dispatch input, which means it appears in the run log where someone
// debugging a failed copy will look.
//
// The inputs are referenced through env rather than interpolated into the
// script. A workflow_dispatch input is a string a person types, and
// "${{ inputs.images }}" placed directly inside a run block is a shell
// injection with extra steps.
const WorkflowYAML = `name: MirrorPilot relay

# Dispatched by MirrorPilot. Commit this file to .github/workflows/ in the
# repository the panel is pointed at, then add two repository secrets:
#
#   ACR_USERNAME  - the username for the target registry
#   ACR_PASSWORD  - the password or access token for it
#
# Nothing else needs setting up. The target prefix and the images arrive as
# inputs, so the values that can write to your registry never leave secrets.

on:
  workflow_dispatch:
    inputs:
      target:
        description: 'Target prefix, e.g. registry.cn-hangzhou.aliyuncs.com/your-namespace'
        required: true
        type: string
      images:
        description: 'Fully qualified image addresses, one per line'
        required: true
        type: string

# Read-only. This workflow copies images; it never touches the repository.
permissions:
  contents: read

jobs:
  relocate:
    runs-on: ubuntu-latest
    timeout-minutes: 30

    steps:
      - name: Log in to the target registry
        env:
          TARGET: ${{ inputs.target }}
          REGISTRY_USERNAME: ${{ secrets.ACR_USERNAME }}
          REGISTRY_PASSWORD: ${{ secrets.ACR_PASSWORD }}
        run: |
          set -euo pipefail
          registry="${TARGET%%/*}"
          printf '%s' "$REGISTRY_PASSWORD" | docker login "$registry" \
            --username "$REGISTRY_USERNAME" --password-stdin

      - name: Copy the images
        env:
          TARGET: ${{ inputs.target }}
          IMAGES: ${{ inputs.images }}
        run: |
          set -euo pipefail
          while IFS= read -r image; do
            [ -n "$image" ] || continue

            # The upstream host is dropped so the path below the target prefix
            # reads the way people expect in a registry: library/alpine:3.21
            # rather than docker.io/library/alpine:3.21.
            name="${image#*/}"
            destination="$TARGET/$name"

            echo "::group::$image"
            docker pull "$image"
            docker tag "$image" "$destination"
            docker push "$destination"
            echo "::endgroup::"

            echo "relocated $image -> $destination"
          done <<< "$IMAGES"
`
