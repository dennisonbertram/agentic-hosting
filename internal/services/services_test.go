package services

import "testing"

func TestValidateImage_AllowsPlatformLocalRegistry(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{name: "docker hub image", image: "nginx:latest"},
		{name: "docker hub namespace image", image: "library/nginx:latest"},
		{name: "loopback registry by ip", image: "127.0.0.1:5000/tenant/my-app:latest"},
		{name: "loopback registry by localhost", image: "localhost:5000/tenant/my-app:latest"},
		{name: "build output image tag", image: "127.0.0.1:5000/ah/tenant-service:build123"},
		{name: "external registry remains blocked", image: "evil.example.com/team/app:latest", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImage(tc.image)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateImage(%q) unexpectedly succeeded", tc.image)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateImage(%q) returned error: %v", tc.image, err)
			}
		})
	}
}
