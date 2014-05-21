class CollectionsController < ApplicationController
  skip_around_filter :thread_with_mandatory_api_token, only: [:show_file]
  skip_before_filter :find_object_by_uuid, only: [:provenance, :show_file]
  skip_before_filter :check_user_agreements, only: [:show_file]

  RELATION_LIMIT = 5

  def show_pane_list
    %w(Files Attributes Metadata Provenance_graph Used_by JSON API)
  end

  def set_persistent
    case params[:value]
    when 'persistent', 'cache'
      persist_links = Link.filter([['owner_uuid', '=', current_user.uuid],
                                   ['link_class', '=', 'resources'],
                                   ['name', '=', 'wants'],
                                   ['tail_uuid', '=', current_user.uuid],
                                   ['head_uuid', '=', @object.uuid]])
      logger.debug persist_links.inspect
    else
      return unprocessable "Invalid value #{value.inspect}"
    end
    if params[:value] == 'persistent'
      if not persist_links.any?
        Link.create(link_class: 'resources',
                    name: 'wants',
                    tail_uuid: current_user.uuid,
                    head_uuid: @object.uuid)
      end
    else
      persist_links.each do |link|
        link.destroy || raise
      end
    end

    respond_to do |f|
      f.json { render json: @object }
    end
  end

  def index
    if params[:search].andand.length.andand > 0
      tags = Link.where(any: ['contains', params[:search]])
      @collections = (Collection.where(uuid: tags.collect(&:head_uuid)) |
                      Collection.where(any: ['contains', params[:search]])).
        uniq { |c| c.uuid }
    else
      if params[:limit]
        limit = params[:limit].to_i
      else
        limit = 100
      end

      if params[:offset]
        offset = params[:offset].to_i
      else
        offset = 0
      end

      @collections = Collection.limit(limit).offset(offset)
    end
    @links = Link.limit(1000).
      where(head_uuid: @collections.collect(&:uuid))
    @collection_info = {}
    @collections.each do |c|
      @collection_info[c.uuid] = {
        tag_links: [],
        wanted: false,
        wanted_by_me: false,
        provenance: [],
        links: []
      }
    end
    @links.each do |link|
      @collection_info[link.head_uuid] ||= {}
      info = @collection_info[link.head_uuid]
      case link.link_class
      when 'tag'
        info[:tag_links] << link
      when 'resources'
        info[:wanted] = true
        info[:wanted_by_me] ||= link.tail_uuid == current_user.uuid
      when 'provenance'
        info[:provenance] << link.name
      end
      info[:links] << link
    end
    @request_url = request.url
  end

  def show_file
    # We pipe from arv-get to send the file to the user.  Before we start it,
    # we ask the API server if the file actually exists.  This serves two
    # purposes: it lets us return a useful status code for common errors, and
    # helps us figure out which token to provide to arv-get.
    coll = nil
    tokens = [Thread.current[:arvados_api_token], params[:reader_token]].compact
    usable_token = find_usable_token(tokens) do
      coll = Collection.find(params[:uuid])
    end
    if usable_token.nil?
      return  # Response already rendered.
    elsif params[:file].nil? or not file_in_collection?(coll, params[:file])
      return render_not_found
    end
    opts = params.merge(arvados_api_token: usable_token)
    ext = File.extname(params[:file])
    self.response.headers['Content-Type'] =
      Rack::Mime::MIME_TYPES[ext] || 'application/octet-stream'
    self.response.headers['Content-Length'] = params[:size] if params[:size]
    self.response.headers['Content-Disposition'] = params[:disposition] if params[:disposition]
    self.response_body = file_enumerator opts
  end

  def show
    return super if !@object
    if current_user
      jobs_with = lambda do |conds|
        Job.limit(RELATION_LIMIT).where(conds)
          .results.sort_by { |j| j.finished_at || j.created_at }
      end
      @output_of = jobs_with.call(output: @object.uuid)
      @log_of = jobs_with.call(log: @object.uuid)
      folder_links = Link.limit(RELATION_LIMIT).order("modified_at DESC")
        .where(head_uuid: @object.uuid, link_class: 'name').results
      folder_hash = Group.where(uuid: folder_links.map(&:tail_uuid)).to_hash
      @folders = folder_links.map { |link| folder_hash[link.tail_uuid] }
      @permissions = Link.limit(RELATION_LIMIT).order("modified_at DESC")
        .where(head_uuid: @object.uuid, link_class: 'permission',
               name: 'can_read').results
      @logs = Log.limit(RELATION_LIMIT).order("created_at DESC")
        .where(object_uuid: @object.uuid).results
      @is_persistent = Link.limit(1)
        .where(head_uuid: @object.uuid, tail_uuid: current_user.uuid,
               link_class: 'resources', name: 'wants')
        .results.any?
    end
    @prov_svg = ProvenanceHelper::create_provenance_graph(@object.provenance, "provenance_svg",
                                                          {:request => request,
                                                            :direction => :bottom_up,
                                                            :combine_jobs => :script_only}) rescue nil
    @used_by_svg = ProvenanceHelper::create_provenance_graph(@object.used_by, "used_by_svg",
                                                             {:request => request,
                                                               :direction => :top_down,
                                                               :combine_jobs => :script_only,
                                                               :pdata_only => true}) rescue nil
  end

  protected

  def find_usable_token(token_list)
    # Iterate over every given token to make it the current token and
    # yield the given block.
    # If the block succeeds, return the token it used.
    # Otherwise, render an error response based on the most specific
    # error we encounter, and return nil.
    most_specific_error = [401]
    token_list.each do |api_token|
      using_specific_api_token(api_token) do
        begin
          yield
          return api_token
        rescue ArvadosApiClient::NotLoggedInException => error
          status = 401
        rescue => error
          status = (error.message =~ /\[API: (\d+)\]$/) ? $1.to_i : nil
          raise unless [401, 403, 404].include?(status)
        end
        if status >= most_specific_error.first
          most_specific_error = [status, error]
        end
      end
    end
    case most_specific_error.shift
    when 401, 403
      redirect_to_login
    when 404
      render_not_found(*most_specific_error)
    end
    return nil
  end

  def file_in_collection?(collection, filename)
    target = CollectionsHelper.file_path(File.split(filename))
    collection.files.each do |file_spec|
      return true if (CollectionsHelper.file_path(file_spec) == target)
    end
    false
  end

  def file_enumerator(opts)
    FileStreamer.new opts
  end

  class FileStreamer
    include ArvadosApiClientHelper
    def initialize(opts={})
      @opts = opts
    end
    def each
      return unless @opts[:uuid] && @opts[:file]
      env = Hash[ENV].
        merge({
                'ARVADOS_API_HOST' =>
                arvados_api_client.arvados_v1_base.
                sub(/\/arvados\/v1/, '').
                sub(/^https?:\/\//, ''),
                'ARVADOS_API_TOKEN' =>
                @opts[:arvados_api_token],
                'ARVADOS_API_HOST_INSECURE' =>
                Rails.configuration.arvados_insecure_https ? 'true' : 'false'
              })
      IO.popen([env, 'arv-get', "#{@opts[:uuid]}/#{@opts[:file]}"],
               'rb') do |io|
        while buf = io.read(2**20)
          yield buf
        end
      end
      Rails.logger.warn("#{@opts[:uuid]}/#{@opts[:file]}: #{$?}") if $? != 0
    end
  end
end
