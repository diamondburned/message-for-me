{
	inputs = {
		nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
		flake-utils.url = "github:numtide/flake-utils";
		flake-compat.url = "github:edolstra/flake-compat";
	};

	outputs = { self, nixpkgs, flake-utils, ... }:
		flake-utils.lib.eachDefaultSystem (system: let
			pkgs = nixpkgs.legacyPackages.${system};
		in
		{
			devShells.default = pkgs.mkShell {
				name = "message-for-me-dev";
				packages = with pkgs; [
					go_1_22
					gopls
					gotools
				];
			};

			packages.default = pkgs.buildGoModule {
				src = self;
				pname = "message-for-me";
				version = self.rev or "latest";

				vendorHash = "sha256-aVCSUhNRqBFrwGAAJs0K3ykLJLy27vN0KxePTc2u4w0=";

				meta = with pkgs.lib; {
					homepage = https://libdb.so/message-for-me;
					mainProgram = "message-for-me";
				};
			};
		});
}
