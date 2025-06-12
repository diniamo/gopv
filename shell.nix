with import <nixpkgs> {}; mkShellNoCC {
  packages = [
    go
    gopls
  ];
}
