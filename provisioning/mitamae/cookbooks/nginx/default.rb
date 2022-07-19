package "nginx"

remote_file "/etc/nginx/nginx.conf" do
  owner "root"
  group "root"
  mode "0644"
  source "nginx.conf"
end

remote_file "/etc/nginx/conf.d/isuports.conf" do
  owner "root"
  group "root"
  mode "0644"
  source "isuports.conf"
end

remote_file "/etc/nginx/conf.d/default.conf" do
  owner "root"
  group "root"
  mode "0644"
  source "default.conf"
end

remote_directory "/etc/nginx/tls" do
  owner "root"
  group "root"
  mode "0755"
  source "tls"
end
