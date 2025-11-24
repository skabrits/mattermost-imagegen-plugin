docker build . -f .\Dockerfile.plugin -t mm-genai-plugin
docker create --name artif  mm-genai-plugin
docker cp artif:/bundle/mattermost-imagegen-plugin.tar.gz ./mattermost-imagegen-plugin.tar.gz
docker rm artif