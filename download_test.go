package download

import (
	"log/slog"
	"testing"
)

func TestManager_Get(t *testing.T) {
	type fields struct {
		conf *Config
	}
	type args struct {
		url string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "Test 1",
			fields: fields{
				conf: &Config{
					Logger: slog.Default(),
				},
			},
			args: args{
				url: "https://updates.cdn-apple.com/2024SpringFCS/fullrestores/062-06652/B7EAA639-CB1A-4D5D-B4B4-11D4DCECFDE9/iPhone15,2_17.5.1_21F90_Restore.ipsw",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, err := New(tt.fields.conf)
			if err != nil {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err := mgr.Get(tt.args.url); (err != nil) != tt.wantErr {
				t.Errorf("Manager.Get() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
